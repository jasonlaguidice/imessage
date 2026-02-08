// nac-relay: Runs on a Mac and serves NAC validation data, contact lookups,
// and chat.db backfill over HTTP. The Linux bridge calls this instead of
// running the x86_64 NAC emulator locally.
//
// Usage:
//   go run tools/nac-relay/main.go [-port 5001] [-addr 0.0.0.0]
//
// Endpoints:
//   POST /validation-data                        → base64-encoded validation data
//   GET  /contact?id=+15551234567                → JSON contact info
//   GET  /contacts                               → all contacts (bulk)
//   GET  /chats?since_days=365                   → recent chats with members
//   GET  /chat-info?guid=<chat_guid>             → single chat info + members
//   GET  /messages?chat_guid=X&since_ts=T        → messages since timestamp (ms)
//   GET  /messages?chat_guid=X&before_ts=T&limit=N → messages before timestamp
//   GET  /attachment?path=~/Library/Messages/...  → raw attachment file
//   GET  /health                                 → "ok"
package main

/*
#cgo CFLAGS: -x objective-c -DNAC_NO_MAIN -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework Contacts

// Inline the validation_data.m source
#include "../../nac-validation/src/validation_data.m"

#import <Contacts/Contacts.h>


// Contact store (initialized once)
static CNContactStore* _contactStore = NULL;
static int _contactAccess = -1; // -1=unknown, 0=denied, 1=granted

static void initContactStore() {
    if (_contactStore) return;
    _contactStore = [[CNContactStore alloc] init];

    CNAuthorizationStatus status = [CNContactStore authorizationStatusForEntityType:CNEntityTypeContacts];
    if (status == CNAuthorizationStatusAuthorized) {
        _contactAccess = 1;
    } else {
        _contactAccess = 0;
    }
}

// requestContactAccess triggers the system prompt. Must be called from
// a background thread (Go goroutine) so the completion handler doesn't
// deadlock the main queue.
static void requestContactAccess() {
    if (!_contactStore) _contactStore = [[CNContactStore alloc] init];
    [_contactStore requestAccessForEntityType:CNEntityTypeContacts completionHandler:^(BOOL granted, NSError *error) {
        _contactAccess = granted ? 1 : 0;
    }];
}

// Contact result struct
typedef struct {
    const char *first_name;
    const char *last_name;
    const char *nickname;
    const char **phones;
    int phone_count;
    const char **emails;
    int email_count;
    int found;
} ContactResult;

static ContactResult lookupContact(const char *identifier) {
    ContactResult r = {0};
    if (_contactAccess != 1 || !identifier) return r;

    @autoreleasepool {
        NSString *idStr = [NSString stringWithUTF8String:identifier];
        NSArray *keysToFetch = @[
            CNContactGivenNameKey, CNContactFamilyNameKey, CNContactNicknameKey,
            CNContactEmailAddressesKey, CNContactPhoneNumbersKey,
        ];

        // Try matching by phone or email across all containers
        NSPredicate *predicate;
        if ([idStr hasPrefix:@"+"] || [idStr length] <= 15) {
            CNPhoneNumber *phone = [CNPhoneNumber phoneNumberWithStringValue:idStr];
            predicate = [CNContact predicateForContactsMatchingPhoneNumber:phone];
        } else {
            predicate = [CNContact predicateForContactsMatchingEmailAddress:idStr];
        }

        NSError *error;
        NSArray<CNContact*> *contacts = [_contactStore unifiedContactsMatchingPredicate:predicate
                                                                           keysToFetch:keysToFetch
                                                                                 error:&error];
        if (!contacts || contacts.count == 0) return r;

        CNContact *c = contacts[0];
        r.found = 1;
        r.first_name = c.givenName ? strdup([c.givenName UTF8String]) : NULL;
        r.last_name = c.familyName ? strdup([c.familyName UTF8String]) : NULL;
        r.nickname = c.nickname ? strdup([c.nickname UTF8String]) : NULL;

        r.phone_count = (int)c.phoneNumbers.count;
        if (r.phone_count > 0) {
            r.phones = (const char **)malloc(sizeof(char*) * r.phone_count);
            for (int i = 0; i < r.phone_count; i++) {
                r.phones[i] = strdup([c.phoneNumbers[i].value.stringValue UTF8String]);
            }
        }

        r.email_count = (int)c.emailAddresses.count;
        if (r.email_count > 0) {
            r.emails = (const char **)malloc(sizeof(char*) * r.email_count);
            for (int i = 0; i < r.email_count; i++) {
                r.emails[i] = strdup([c.emailAddresses[i].value UTF8String]);
            }
        }
    }
    return r;
}

static void freeContactResult(ContactResult *r) {
    if (r->first_name) free((void*)r->first_name);
    if (r->last_name) free((void*)r->last_name);
    if (r->nickname) free((void*)r->nickname);
    for (int i = 0; i < r->phone_count; i++) free((void*)r->phones[i]);
    for (int i = 0; i < r->email_count; i++) free((void*)r->emails[i]);
    if (r->phones) free(r->phones);
    if (r->emails) free(r->emails);
}

static int getContactAccess() { return _contactAccess; }
static int getContactAuthStatus() {
    return (int)[CNContactStore authorizationStatusForEntityType:CNEntityTypeContacts];
}

static void recheckContactAccess() {
    CNAuthorizationStatus status = [CNContactStore authorizationStatusForEntityType:CNEntityTypeContacts];
    _contactAccess = (status == CNAuthorizationStatusAuthorized) ? 1 : 0;
}





// Bulk fetch all contacts
typedef struct {
    ContactResult *contacts;
    int count;
} ContactList;

static ContactList getAllContacts() {
    ContactList list = {0};
    if (_contactAccess != 1) return list;

    @autoreleasepool {
        NSArray *keysToFetch = @[
            CNContactGivenNameKey, CNContactFamilyNameKey, CNContactNicknameKey,
            CNContactEmailAddressesKey, CNContactPhoneNumbersKey,
        ];
        NSError *error;

        // Fetch from ALL containers (iCloud, Gmail, Exchange, local, etc.)
        NSMutableArray<CNContact*> *allContacts = [NSMutableArray array];
        NSArray<CNContainer*> *containers = [_contactStore containersMatchingPredicate:nil error:&error];
        for (CNContainer *container in containers) {
            NSPredicate *predicate = [CNContact predicateForContactsInContainerWithIdentifier:container.identifier];
            NSArray<CNContact*> *contacts = [_contactStore unifiedContactsMatchingPredicate:predicate
                                                                               keysToFetch:keysToFetch
                                                                                     error:&error];
            if (contacts) [allContacts addObjectsFromArray:contacts];
        }
        // Deduplicate by contact identifier (unified contacts appear in multiple containers)
        NSMutableDictionary<NSString*, CNContact*> *uniqueMap = [NSMutableDictionary dictionary];
        for (CNContact *c in allContacts) {
            if (!uniqueMap[c.identifier]) {
                uniqueMap[c.identifier] = c;
            }
        }
        NSArray<CNContact*> *contacts = uniqueMap.allValues;
        if (contacts.count == 0) return list;

        list.count = (int)contacts.count;
        list.contacts = (ContactResult *)calloc(list.count, sizeof(ContactResult));

        for (int i = 0; i < list.count; i++) {
            CNContact *c = contacts[i];
            list.contacts[i].found = 1;
            list.contacts[i].first_name = c.givenName.length > 0 ? strdup([c.givenName UTF8String]) : NULL;
            list.contacts[i].last_name = c.familyName.length > 0 ? strdup([c.familyName UTF8String]) : NULL;
            list.contacts[i].nickname = c.nickname.length > 0 ? strdup([c.nickname UTF8String]) : NULL;

            list.contacts[i].phone_count = (int)c.phoneNumbers.count;
            if (list.contacts[i].phone_count > 0) {
                list.contacts[i].phones = (const char **)malloc(sizeof(char*) * list.contacts[i].phone_count);
                for (int j = 0; j < list.contacts[i].phone_count; j++) {
                    list.contacts[i].phones[j] = strdup([c.phoneNumbers[j].value.stringValue UTF8String]);
                }
            }

            list.contacts[i].email_count = (int)c.emailAddresses.count;
            if (list.contacts[i].email_count > 0) {
                list.contacts[i].emails = (const char **)malloc(sizeof(char*) * list.contacts[i].email_count);
                for (int j = 0; j < list.contacts[i].email_count; j++) {
                    list.contacts[i].emails[j] = strdup([c.emailAddresses[j].value UTF8String]);
                }
            }
        }
    }
    return list;
}

static void freeContactList(ContactList *list) {
    for (int i = 0; i < list->count; i++) {
        freeContactResult(&list->contacts[i]);
    }
    free(list->contacts);
}

// ============================================================================
// attributedBody decoder — extracts text from NSKeyedArchive blobs
// ============================================================================

static id jsonSafeObject(id obj);

static NSArray* jsonSafeArray(NSArray* input) {
    NSMutableArray* output = [[NSMutableArray alloc] initWithCapacity:input.count];
    for (NSUInteger i = 0; i < input.count; i++) {
        [output addObject:jsonSafeObject(input[i])];
    }
    return output;
}

static NSDictionary* jsonSafeDict(NSDictionary* input) {
    NSMutableDictionary* output = [[NSMutableDictionary alloc] init];
    [input enumerateKeysAndObjectsUsingBlock:^(id key, id obj, BOOL *stop) {
        [output setObject:jsonSafeObject(obj) forKey:key];
    }];
    return output;
}

static id jsonSafeObject(id obj) {
    if ([obj isKindOfClass:[NSString class]] || [obj isKindOfClass:[NSNumber class]] || [obj isKindOfClass:[NSNull class]]) {
        return obj;
    } else if ([obj isKindOfClass:[NSData class]]) {
        return [(NSData*)obj base64EncodedStringWithOptions:0];
    } else if ([obj isKindOfClass:[NSURL class]]) {
        return ((NSURL*)obj).absoluteString;
    } else if ([obj isKindOfClass:[NSDictionary class]]) {
        return jsonSafeDict(obj);
    } else if ([obj isKindOfClass:[NSArray class]]) {
        return jsonSafeArray(obj);
    }
    return @"unknown object";
}

// decodeAttributedBody takes raw attributedBody bytes and returns a JSON string
// with {"content":"...","attributes":[...]} or an error string.
static char* decodeAttributedBody(const void *bytes, int length) {
    @try {
        @autoreleasepool {
            NSData *data = [NSData dataWithBytes:bytes length:length];
            #pragma clang diagnostic push
            #pragma clang diagnostic ignored "-Wdeprecated-declarations"
            NSUnarchiver *arch = [[NSUnarchiver alloc] initForReadingWithData:data];
            NSAttributedString *str = [arch decodeObject];
            #pragma clang diagnostic pop

            if (!str || str.length == 0) return strdup("");

            NSMutableArray *attrs = [[NSMutableArray alloc] init];
            [str enumerateAttributesInRange:NSMakeRange(0, [str length])
                                   options:NSAttributedStringEnumerationLongestEffectiveRangeNotRequired
                                usingBlock:^(NSDictionary *attributes, NSRange range, BOOL *stop) {
                [attrs addObject:@{
                    @"location": [NSNumber numberWithUnsignedInteger:range.location],
                    @"length": [NSNumber numberWithUnsignedInteger:range.length],
                    @"values": jsonSafeDict(attributes),
                }];
            }];

            NSDictionary *outputDict = @{
                @"content": str.string,
                @"attributes": attrs,
            };
            NSError *error = NULL;
            NSData *jsonData = [NSJSONSerialization dataWithJSONObject:outputDict options:0 error:&error];
            if (!jsonData && error) return strdup("");
            NSString *jsonString = [[NSString alloc] initWithData:jsonData encoding:NSUTF8StringEncoding];
            return strdup([jsonString UTF8String]);
        }
    }
    @catch (NSException *err) {
        return strdup("");
    }
}
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

// decodeAttributedBodyJSON decodes an NSKeyedArchive attributedBody blob
// and returns the JSON {"content":"...","attributes":[...]} representation.
func decodeAttributedBodyJSON(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	cResult := C.decodeAttributedBody(unsafe.Pointer(&data[0]), C.int(len(data)))
	if cResult == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cResult))
	return C.GoString(cResult)
}

var nacMu sync.Mutex // serialize NAC calls (framework may not be thread-safe)

func generateValidationData() ([]byte, error) {
	nacMu.Lock()
	defer nacMu.Unlock()

	var buf *C.uint8_t
	var bufLen C.size_t
	var errBuf *C.char

	result := C.nac_generate_validation_data(&buf, &bufLen, &errBuf)
	if result != 0 {
		errMsg := "unknown error"
		if errBuf != nil {
			errMsg = C.GoString(errBuf)
			C.free(unsafe.Pointer(errBuf))
		}
		return nil, fmt.Errorf("NAC error %d: %s", result, errMsg)
	}

	data := C.GoBytes(unsafe.Pointer(buf), C.int(bufLen))
	C.free(unsafe.Pointer(buf))
	return data, nil
}

// ContactInfo matches the bridge's imessage.Contact struct
type ContactInfo struct {
	FirstName string   `json:"first_name,omitempty"`
	LastName  string   `json:"last_name,omitempty"`
	Nickname  string   `json:"nickname,omitempty"`
	Phones    []string `json:"phones,omitempty"`
	Emails    []string `json:"emails,omitempty"`
}

var contactMu sync.Mutex

func lookupContact(identifier string) *ContactInfo {
	contactMu.Lock()
	defer contactMu.Unlock()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cID := C.CString(identifier)
	defer C.free(unsafe.Pointer(cID))

	r := C.lookupContact(cID)
	defer C.freeContactResult(&r)

	if r.found == 0 {
		return nil
	}

	info := &ContactInfo{}
	if r.first_name != nil {
		info.FirstName = C.GoString(r.first_name)
	}
	if r.last_name != nil {
		info.LastName = C.GoString(r.last_name)
	}
	if r.nickname != nil {
		info.Nickname = C.GoString(r.nickname)
	}
	for i := 0; i < int(r.phone_count); i++ {
		info.Phones = append(info.Phones, C.GoString(*(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(r.phones)) + uintptr(i)*unsafe.Sizeof(r.phones)))))
	}
	for i := 0; i < int(r.email_count); i++ {
		info.Emails = append(info.Emails, C.GoString(*(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(r.emails)) + uintptr(i)*unsafe.Sizeof(r.emails)))))
	}
	return info
}

func main() {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "nac-relay must run on macOS")
		os.Exit(1)
	}

	addr := flag.String("addr", "0.0.0.0", "Address to bind to")
	port := flag.Int("port", 5001, "Port to listen on")
	setup := flag.Bool("setup", false, "Install .app bundle and LaunchAgent, then start service")
	flag.Parse()

	if *setup {
		runSetup()
		return
	}

	// Wait for Full Disk Access before doing anything else — same as main app.
	// FDA must be granted first; contacts prompt only works after FDA is set.
	db, err := openChatDB()
	if err != nil {
		log.Println("Full Disk Access not granted — prompting user")
		promptForFDA()
		log.Println("Waiting for Full Disk Access...")
		for {
			db, err = openChatDB()
			if err == nil {
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
	db.Close()
	log.Println("Full Disk Access granted")

	// Request contact access — same pattern as main app:
	// Check status first, then request from a goroutine (background thread)
	// so the completion handler doesn't deadlock.
	authStatus := int(C.getContactAuthStatus())
	if authStatus == 3 { // CNAuthorizationStatusAuthorized
		C.initContactStore()
		log.Println("Contacts access: granted")
	} else if authStatus == 0 { // CNAuthorizationStatusNotDetermined
		log.Println("Requesting Contacts access...")
		done := make(chan struct{})
		go func() {
			C.requestContactAccess()
			// Poll until callback sets the result
			for C.getContactAccess() == -1 {
				time.Sleep(100 * time.Millisecond)
			}
			close(done)
		}()
		// Wait up to 30s for user to respond to prompt
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			log.Println("Contacts prompt timed out")
		}
		C.recheckContactAccess()
		if C.getContactAccess() == 1 {
			log.Println("Contacts access: granted")
		} else {
			log.Println("Contacts access: denied")
		}
	} else {
		C.initContactStore()
		log.Println("Contacts access: denied (contact lookups will return empty)")
		log.Println("Grant in System Settings → Privacy & Security → Contacts")
	}

	// Initialize chat.db for backfill
	registerChatDBEndpoints()

	// Test that NAC works on startup
	log.Println("Testing NAC validation data generation...")
	start := time.Now()
	vd, err := generateValidationData()
	if err != nil {
		log.Fatalf("NAC test failed: %v", err)
	}
	log.Printf("NAC test OK: %d bytes in %v", len(vd), time.Since(start))

	http.HandleFunc("/validation-data", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		start := time.Now()
		data, err := generateValidationData()
		if err != nil {
			log.Printf("ERROR: NAC generation failed: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b64 := base64.StdEncoding.EncodeToString(data)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(b64))
		log.Printf("Served %d bytes of validation data in %v (from %s)",
			len(data), time.Since(start), r.RemoteAddr)
	})

	http.HandleFunc("/contacts", func(w http.ResponseWriter, r *http.Request) {
		contactMu.Lock()
		runtime.LockOSThread()

		cList := C.getAllContacts()

		var contacts []ContactInfo
		for i := 0; i < int(cList.count); i++ {
			cr := cList.contacts
			// Pointer arithmetic to get the i-th element
			ptr := (*C.ContactResult)(unsafe.Pointer(uintptr(unsafe.Pointer(cr)) + uintptr(i)*unsafe.Sizeof(*cr)))
			if ptr.found == 0 {
				continue
			}
			info := ContactInfo{}
			if ptr.first_name != nil {
				info.FirstName = C.GoString(ptr.first_name)
			}
			if ptr.last_name != nil {
				info.LastName = C.GoString(ptr.last_name)
			}
			if ptr.nickname != nil {
				info.Nickname = C.GoString(ptr.nickname)
			}
			for j := 0; j < int(ptr.phone_count); j++ {
				p := *(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(ptr.phones)) + uintptr(j)*unsafe.Sizeof(ptr.phones)))
				info.Phones = append(info.Phones, C.GoString(p))
			}
			for j := 0; j < int(ptr.email_count); j++ {
				e := *(**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(ptr.emails)) + uintptr(j)*unsafe.Sizeof(ptr.emails)))
				info.Emails = append(info.Emails, C.GoString(e))
			}
			if info.FirstName != "" || info.LastName != "" || info.Nickname != "" {
				contacts = append(contacts, info)
			}
		}

		C.freeContactList(&cList)
		runtime.UnlockOSThread()
		contactMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(contacts)
		log.Printf("Served %d contacts", len(contacts))
	})

	http.HandleFunc("/contact", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing ?id= parameter", http.StatusBadRequest)
			return
		}
		contact := lookupContact(id)
		w.Header().Set("Content-Type", "application/json")
		if contact == nil {
			w.Write([]byte("null"))
			return
		}
		json.NewEncoder(w).Encode(contact)
		log.Printf("Contact lookup: %s → %s %s", id, contact.FirstName, contact.LastName)
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	listenAddr := fmt.Sprintf("%s:%d", *addr, *port)
	log.Printf("NAC relay listening on %s", listenAddr)

	// Print helpful info
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
				log.Printf("  → Bridge relay URL: http://%s:%d/validation-data", ipnet.IP, *port)
			}
		}
	}
	log.Println("Use -relay <url> when running extract-key to embed this URL in the hardware key.")

	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
