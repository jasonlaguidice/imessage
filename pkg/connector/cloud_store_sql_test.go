package connector

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// These tests exercise the store SQL against real databases (SQLite always,
// PostgreSQL when IMESSAGE_TEST_PG_URI is set; see store_testdb_test.go).
// Modelled on upstream's cloud_store_sql_test.go (ed4c27dd, 659e03ba).
//
// The rest of this package's tests are pure logic, so a SQLite-only spelling
// (literal "?" on a raw *sql.Tx, ORDER BY rowid) could survive until it hit a
// Postgres bridge in production.

const (
	testStoreLoginA = networkid.UserLoginID("login-a")
	testStoreLoginB = networkid.UserLoginID("login-b")
)

func newTestCloudStore(t *testing.T, db *dbutil.Database, loginID networkid.UserLoginID) *cloudBackfillStore {
	t.Helper()
	store := newCloudBackfillStore(db, loginID)
	if err := store.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	return store
}

// TestStoreEnsureSchemaIsIdempotent runs every store's migrations twice; the
// second pass must not try to re-add columns that already exist.
func TestStoreEnsureSchemaIsIdempotent(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		backfill := newCloudBackfillStore(db, testStoreLoginA)
		profiles := newSharedProfileStore(db, testStoreLoginA)
		pending := newPendingAttachmentStore(db, testStoreLoginA)
		for pass := 1; pass <= 2; pass++ {
			if err := backfill.ensureSchema(ctx); err != nil {
				t.Fatalf("cloudBackfillStore.ensureSchema pass %d: %v", pass, err)
			}
			if err := profiles.ensureSchema(ctx); err != nil {
				t.Fatalf("sharedProfileStore.ensureSchema pass %d: %v", pass, err)
			}
			if err := pending.ensureSchema(ctx); err != nil {
				t.Fatalf("pendingAttachmentStore.ensureSchema pass %d: %v", pass, err)
			}
		}
	})
}

func TestStoreSQLPlaceholders(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		got := sqlPlaceholders(db, 3)
		want := "?, ?, ?"
		if db.Dialect == dbutil.Postgres {
			want = "$1, $2, $3"
		}
		if got != want {
			t.Errorf("sqlPlaceholders(3) = %q, want %q", got, want)
		}
	})
}

// TestStoreUpsertMessageBatch covers the raw-transaction batch insert and its
// ON CONFLICT update path, including the sticky-deleted and max-date_read
// rules, and checks that rows stay scoped to their login.
func TestStoreUpsertMessageBatch(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		store := newTestCloudStore(t, db, testStoreLoginA)
		other := newCloudBackfillStore(db, testStoreLoginB)
		tapback := uint32(2000)

		rows := []cloudMessageRow{
			{GUID: "m1", RecordName: "r1", CloudChatID: "chat1", PortalID: "p1", TimestampMS: 1000, Sender: "tel:+1555", Text: "hello", Service: "iMessage", HasBody: true},
			{GUID: "m2", RecordName: "r2", CloudChatID: "chat1", PortalID: "p1", TimestampMS: 2000, IsFromMe: true, Text: "hi", Service: "iMessage", HasBody: true, DateReadMS: 5000},
			{GUID: "m3", RecordName: "r3", CloudChatID: "chat1", PortalID: "p1", TimestampMS: 3000, Sender: "tel:+1555", TapbackType: &tapback, TapbackTargetGUID: "m2", HasBody: true},
			// Duplicate GUID within the batch: the newer row wins.
			{GUID: "m1", RecordName: "r1", CloudChatID: "chat1", PortalID: "p1", TimestampMS: 1001, Sender: "tel:+1555", Text: "hello edited", Service: "iMessage", HasBody: true},
		}
		if err := store.upsertMessageBatch(ctx, rows); err != nil {
			t.Fatalf("upsertMessageBatch: %v", err)
		}

		got, err := store.listLatestMessages(ctx, "p1", 10)
		if err != nil {
			t.Fatalf("listLatestMessages: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("listLatestMessages returned %d rows, want 3", len(got))
		}
		if got[0].GUID != "m3" || got[0].TapbackType == nil || *got[0].TapbackType != tapback {
			t.Errorf("newest row = %+v, want tapback m3", got[0])
		}
		if got[2].GUID != "m1" || got[2].Text != "hello edited" || got[2].TimestampMS != 1001 {
			t.Errorf("oldest row = %+v, want deduplicated m1", got[2])
		}

		// Conflict path: m2 is soft-deleted locally and must stay deleted;
		// an older date_read_ms must not overwrite a newer one.
		store.softDeleteMessageByGUID(ctx, "m2")
		if err := store.upsertMessageBatch(ctx, []cloudMessageRow{
			{GUID: "m2", RecordName: "r2", CloudChatID: "chat1", PortalID: "p1", TimestampMS: 2000, IsFromMe: true, Text: "hi again", Service: "iMessage", HasBody: true, DateReadMS: 10},
		}); err != nil {
			t.Fatalf("upsertMessageBatch (conflict): %v", err)
		}
		var deleted bool
		var dateRead int64
		var text string
		if err := db.QueryRow(ctx,
			`SELECT deleted, date_read_ms, text FROM cloud_message WHERE login_id=$1 AND guid=$2`,
			testStoreLoginA, "m2",
		).Scan(&deleted, &dateRead, &text); err != nil {
			t.Fatalf("read back m2: %v", err)
		}
		if !deleted || dateRead != 5000 || text != "hi again" {
			t.Errorf("m2 after conflict: deleted=%v date_read_ms=%d text=%q; want true, 5000, %q", deleted, dateRead, text, "hi again")
		}

		existing, err := store.hasMessageBatch(ctx, []string{"m1", "m2", "nope"})
		if err != nil {
			t.Fatalf("hasMessageBatch: %v", err)
		}
		if !existing["m1"] || !existing["m2"] || existing["nope"] {
			t.Errorf("hasMessageBatch = %v", existing)
		}
		if n, err := other.countBackfillableMessages(ctx, "p1", false); err != nil || n != 0 {
			t.Errorf("other login sees %d messages (err=%v), want 0", n, err)
		}
	})
}

// TestStoreUpsertChatBatch covers the raw-transaction chat upsert and the
// tombstone insert, both of which used literal "?" placeholders.
func TestStoreUpsertChatBatch(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		store := newTestCloudStore(t, db, testStoreLoginA)

		chats := []cloudChatUpsertRow{
			{CloudChatID: "chat1", RecordName: "cr1", PortalID: "tel:+1555", Service: "iMessage", DisplayName: nil, GroupPhotoGuid: nil, ParticipantsJSON: `["tel:+1555"]`, UpdatedTS: 100},
			{CloudChatID: "chat2", RecordName: "cr2", GroupID: "G1", PortalID: "gid:g1", Service: "iMessage", DisplayName: "Group", GroupPhotoGuid: "photo1", ParticipantsJSON: `["tel:+1555","tel:+1666"]`, UpdatedTS: 100, IsFiltered: 1},
		}
		if err := store.upsertChatBatch(ctx, chats); err != nil {
			t.Fatalf("upsertChatBatch: %v", err)
		}
		// Older update must not overwrite the display name.
		if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{
			{CloudChatID: "chat2", RecordName: "cr2", GroupID: "G1", PortalID: "gid:g1", Service: "iMessage", DisplayName: "Stale", ParticipantsJSON: `[]`, UpdatedTS: 50},
		}); err != nil {
			t.Fatalf("upsertChatBatch (conflict): %v", err)
		}
		if name, err := store.getDisplayNameByPortalID(ctx, "gid:g1"); err != nil || name != "Group" {
			t.Errorf("getDisplayNameByPortalID = %q, %v; want Group", name, err)
		}
		if portal, err := store.getChatPortalID(ctx, "chat1"); err != nil || portal != "tel:+1555" {
			t.Errorf("getChatPortalID = %q, %v", portal, err)
		}
		have, err := store.hasChatBatch(ctx, []string{"chat1", "chat2", "chat3"})
		if err != nil {
			t.Fatalf("hasChatBatch: %v", err)
		}
		if !have["chat1"] || !have["chat2"] || have["chat3"] {
			t.Errorf("hasChatBatch = %v", have)
		}

		if err := store.insertDeletedChatTombstone(ctx, "chat3", "gid:g3", "cr3", "G3", "iMessage", nil, `[]`); err != nil {
			t.Fatalf("insertDeletedChatTombstone: %v", err)
		}
		// Second insert hits ON CONFLICT DO NOTHING.
		if err := store.insertDeletedChatTombstone(ctx, "chat3", "gid:g3", "cr3", "G3", "iMessage", nil, `[]`); err != nil {
			t.Fatalf("insertDeletedChatTombstone (repeat): %v", err)
		}
		deleted, err := store.portalIsExplicitlyDeleted(ctx, "gid:g3")
		if err != nil || !deleted {
			t.Errorf("portalIsExplicitlyDeleted(gid:g3) = %v, %v; want true", deleted, err)
		}
	})
}

// TestStoreGetConversationReadByMe checks the replacement for ORDER BY rowid:
// rows tied on timestamp_ms (and created_ts, since one batch stamps them all
// identically) are broken by guid DESC, deterministically on both dialects.
func TestStoreGetConversationReadByMe(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		store := newTestCloudStore(t, db, testStoreLoginA)
		tapback := uint32(2000)

		if err := store.upsertMessageBatch(ctx, []cloudMessageRow{
			{GUID: "a", RecordName: "ra", PortalID: "tied-mine", TimestampMS: 1000, IsFromMe: false, Text: "x", HasBody: true},
			{GUID: "b", RecordName: "rb", PortalID: "tied-mine", TimestampMS: 1000, IsFromMe: true, Text: "y", HasBody: true},

			{GUID: "c", RecordName: "rc", PortalID: "tied-theirs", TimestampMS: 1000, IsFromMe: true, Text: "x", HasBody: true},
			{GUID: "d", RecordName: "rd", PortalID: "tied-theirs", TimestampMS: 1000, IsFromMe: false, Text: "y", HasBody: true},

			// Higher guid inserted first: guid DESC picks h (outgoing), where
			// the old rowid DESC would have picked g (incoming).
			{GUID: "h", RecordName: "rh", PortalID: "tied-reverse", TimestampMS: 1000, IsFromMe: true, Text: "x", HasBody: true},
			{GUID: "g", RecordName: "rg", PortalID: "tied-reverse", TimestampMS: 1000, IsFromMe: false, Text: "y", HasBody: true},

			// A newer incoming tapback does not make the thread unread.
			{GUID: "e", RecordName: "re", PortalID: "tapback", TimestampMS: 1000, IsFromMe: true, Text: "x", HasBody: true},
			{GUID: "f", RecordName: "rf", PortalID: "tapback", TimestampMS: 2000, IsFromMe: false, TapbackType: &tapback, TapbackTargetGUID: "e", HasBody: true},
		}); err != nil {
			t.Fatalf("upsertMessageBatch: %v", err)
		}
		if err := store.upsertChatBatch(ctx, []cloudChatUpsertRow{
			{CloudChatID: "chat-empty", PortalID: "empty", Service: "iMessage", ParticipantsJSON: `[]`, UpdatedTS: 1},
		}); err != nil {
			t.Fatalf("upsertChatBatch: %v", err)
		}

		for portal, want := range map[string]bool{
			"tied-mine":    true,
			"tied-theirs":  false,
			"tied-reverse": true,
			"tapback":      true,
			"empty":        true,
			"unknown":      false,
		} {
			for i := 0; i < 3; i++ {
				got, err := store.getConversationReadByMe(ctx, portal)
				if err != nil {
					t.Fatalf("getConversationReadByMe(%s): %v", portal, err)
				}
				if got != want {
					t.Errorf("getConversationReadByMe(%s) = %v, want %v", portal, got, want)
				}
			}
		}
	})
}

// TestStoreQueriesRunOnDialect is a broad smoke test: it calls most of the
// store's read and write paths once with realistic rows and fails on any SQL
// error. Its purpose is to catch dialect-specific syntax, not to re-verify
// each method's semantics.
func TestStoreQueriesRunOnDialect(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		store := newTestCloudStore(t, db, testStoreLoginA)
		must := func(name string, err error) {
			t.Helper()
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}

		token := "tok"
		must("setSyncStateSuccess", store.setSyncStateSuccess(ctx, cloudZoneMessages, &token))
		_, err := store.getSyncState(ctx, cloudZoneMessages)
		must("getSyncState", err)
		must("setSyncStateError", store.setSyncStateError(ctx, cloudZoneChats, "boom"))
		must("setSyncVersion", store.setSyncVersion(ctx, 3))
		_, err = store.getSyncVersion(ctx)
		must("getSyncVersion", err)
		must("setChatSyncVersion", store.setChatSyncVersion(ctx, 2))
		_, err = store.getChatSyncVersion(ctx)
		must("getChatSyncVersion", err)
		_, err = store.hasAnySyncState(ctx)
		must("hasAnySyncState", err)

		name := "Group"
		must("upsertChat", store.upsertChat(ctx, "chatG", "crG", "G1", "gid:g1", "iMessage", &name, nil, []string{"tel:+1555", "tel:+1666"}, 10))
		must("upsertChat dm", store.upsertChat(ctx, "chatD", "crD", "", "tel:+1555", "iMessage", nil, nil, []string{"tel:+1555"}, 10))
		attJSON := `[{"guid":"att1","record_name":"attrec1","file_size":1}]`
		must("upsertMessageBatch", store.upsertMessageBatch(ctx, []cloudMessageRow{
			{GUID: "g1", RecordName: "rg1", CloudChatID: "chatG", PortalID: "gid:g1", TimestampMS: 1000, Sender: "tel:+1555", Text: "hi", HasBody: true, AttachmentsJSON: attJSON},
			{GUID: "g2", RecordName: "rg2", CloudChatID: "chatG", PortalID: "gid:g1", TimestampMS: 2000, IsFromMe: true, Text: "yo", HasBody: true},
			{GUID: "d1", RecordName: "rd1", CloudChatID: "chatD", PortalID: "tel:+1555", TimestampMS: 1500, Sender: "tel:+1555", Text: "dm", HasBody: true},
			{GUID: "x1", RecordName: "rx1", CloudChatID: "chat1;+;x", PortalID: "tel:+1777", TimestampMS: 1500, Sender: "tel:+1777", Text: "misrouted", HasBody: true},
		}))
		must("upsertChat sms", store.upsertChat(ctx, "chatS", "crS", "", "tel:+1888", "SMS", nil, nil, []string{"tel:+1888", "tel:+1999"}, 10))

		_, err = store.listCarrierGroupChats(ctx)
		must("listCarrierGroupChats", err)
		must("reKeyPortalID", store.reKeyPortalID(ctx, "tel:+1777", "tel:+1778"))

		_, err = store.lookupPortalIDsByRecordNames(ctx, []string{"crG", "crD"})
		must("lookupPortalIDsByRecordNames", err)
		_, err = store.classifyRecycleBinPortals(ctx, []string{"g1", "d1"})
		must("classifyRecycleBinPortals", err)
		must("ensureDeletedChatTombstoneByPortalID", store.ensureDeletedChatTombstoneByPortalID(ctx, "tel:+1555"))
		_, err = store.listDeletedPortalIDs(ctx)
		must("listDeletedPortalIDs", err)
		must("setRestoreOverride", store.setRestoreOverride(ctx, "gid:g1"))
		_, err = store.hasRestoreOverride(ctx, "gid:g1")
		must("hasRestoreOverride", err)
		_ = store.listRestoreOverrides(ctx)
		must("clearRestoreOverride", store.clearRestoreOverride(ctx, "gid:g1"))
		_, err = store.portalHasChat(ctx, "gid:g1")
		must("portalHasChat", err)
		_, err = store.getSoftDeletedPortalInfo(ctx, "gid:g1")
		must("getSoftDeletedPortalInfo", err)
		_, err = store.hasChat(ctx, "chatG")
		must("hasChat", err)
		if rec, err := store.getAttachmentRecordName(ctx, "g1", 0); err != nil || rec != "attrec1" {
			t.Errorf("getAttachmentRecordName = %q, %v", rec, err)
		}
		_, err = store.getChatParticipantsByPortalID(ctx, "gid:g1")
		must("getChatParticipantsByPortalID", err)
		must("updateChatParticipants", store.updateChatParticipants(ctx, "gid:g1", []string{"tel:+1555", "tel:+1666", "tel:+1888"}))
		_ = store.getChatIdentifierByPortalID(ctx, "gid:g1")
		_, err = store.getCloudRecordNamesByPortalID(ctx, "gid:g1")
		must("getCloudRecordNamesByPortalID", err)
		_, err = store.getMessageRecordNamesByPortalID(ctx, "gid:g1")
		must("getMessageRecordNamesByPortalID", err)
		_, err = store.getCloudRecordNamesByGroupID(ctx, "g1")
		must("getCloudRecordNamesByGroupID", err)
		_, err = store.findPortalIDsByGroupID(ctx, "g1")
		must("findPortalIDsByGroupID", err)
		_ = store.getGroupIDForPortalID(ctx, "gid:g1")
		_, err = store.normalizeGroupChatPortalIDs(ctx)
		must("normalizeGroupChatPortalIDs", err)
		_, err = store.normalizeGroupMessagePortalIDs(ctx)
		must("normalizeGroupMessagePortalIDs", err)
		_, err = store.getMessageRecordNamesByGroupID(ctx, "g1")
		must("getMessageRecordNamesByGroupID", err)
		_ = store.getRecordNameByGUID(ctx, "g1")
		_, _, err = store.getGroupPhotoByPortalID(ctx, "gid:g1")
		must("getGroupPhotoByPortalID", err)
		must("saveGroupPhoto", store.saveGroupPhoto(ctx, "gid:g1", 5, []byte{0, 1, 2, 0xff}))
		if _, data, err := store.getGroupPhoto(ctx, "gid:g1"); err != nil || len(data) != 4 || data[3] != 0xff {
			t.Errorf("getGroupPhoto = %v, %v", data, err)
		}
		must("clearGroupPhoto", store.clearGroupPhoto(ctx, "gid:g1"))
		must("clearGroupPhotoGuid", store.clearGroupPhotoGuid(ctx, "gid:g1"))
		must("updateDisplayNameByPortalID", store.updateDisplayNameByPortalID(ctx, "gid:g1", "Renamed"))
		must("persistMessageUUID", store.persistMessageUUID(ctx, "u1", "gid:g1", 3000, true))
		must("persistTapbackUUID", store.persistTapbackUUID(ctx, "u2", "gid:g1", 3001, false, 2001))
		_, err = store.hasMessageUUID(ctx, "U1")
		must("hasMessageUUID", err)
		_, err = store.getMessageTextByGUID(ctx, "g1")
		must("getMessageTextByGUID", err)
		_, _, err = store.getMessageTimestampByGUID(ctx, "g1")
		must("getMessageTimestampByGUID", err)
		_, err = store.portalHasPreStartupOutgoingMessages(ctx, "gid:g1", 5000)
		must("portalHasPreStartupOutgoingMessages", err)
		_, err = store.findPortalIDsByParticipants(ctx, []string{"tel:+1555"}, nil)
		must("findPortalIDsByParticipants", err)
		_, err = store.getOldestMessageTimestamp(ctx, "gid:g1")
		must("getOldestMessageTimestamp", err)
		_, err = store.getNewestMessageTimestamp(ctx, "gid:g1")
		must("getNewestMessageTimestamp", err)
		_, err = store.getNewestBackfillableMessageTimestamp(ctx, "gid:g1", true)
		must("getNewestBackfillableMessageTimestamp", err)
		_, err = store.healMisroutedGroupMessages(ctx)
		must("healMisroutedGroupMessages", err)
		_, err = store.hasPortalMessages(ctx, "gid:g1")
		must("hasPortalMessages", err)
		_, err = store.hasContentfulMessages(ctx, "gid:g1")
		must("hasContentfulMessages", err)
		_, err = store.countBackfillableMessages(ctx, "gid:g1", true)
		must("countBackfillableMessages", err)
		_, err = store.listBackwardMessages(ctx, "gid:g1", 5000, "zz", 10)
		must("listBackwardMessages", err)
		_, err = store.listForwardMessages(ctx, "gid:g1", 0, "", 10)
		must("listForwardMessages", err)
		_, err = store.listOldestMessages(ctx, "gid:g1", 10)
		must("listOldestMessages", err)
		_, err = store.listAllAttachmentMessages(ctx)
		must("listAllAttachmentMessages", err)
		_, err = store.listPortalIDsWithNewestTimestamp(ctx)
		must("listPortalIDsWithNewestTimestamp", err)
		_, err = store.debugMessageStats(ctx, "gid:g1")
		must("debugMessageStats", err)
		_, err = store.debugFindPortalsByIdentifierSuffix(ctx, "1555")
		must("debugFindPortalsByIdentifierSuffix", err)
		_, err = store.debugChatInfo(ctx, "gid:g1")
		must("debugChatInfo", err)
		_, err = store.debugTotalMessageCount(ctx)
		must("debugTotalMessageCount", err)
		_, err = store.getCloudChatRecordByPortalID(ctx, "gid:g1")
		must("getCloudChatRecordByPortalID", err)
		_, err = store.hasCloudReadReceipt(ctx, "G2")
		must("hasCloudReadReceipt", err)
		_, err = store.isCloudBackfilledMessage(ctx, "G1")
		must("isCloudBackfilledMessage", err)

		store.markForwardBackfillDone(ctx, "gid:g1")
		store.markForwardBackfillDone(ctx, "no-chat-row")
		if !store.isForwardBackfillDone(ctx, "gid:g1") || !store.isForwardBackfillDone(ctx, "no-chat-row") {
			t.Errorf("markForwardBackfillDone did not persist")
		}
		done, err := store.getForwardBackfillDonePortals(ctx)
		must("getForwardBackfillDonePortals", err)
		if !done["gid:g1"] || !done["no-chat-row"] {
			t.Errorf("getForwardBackfillDonePortals = %v", done)
		}
		must("resetForwardBackfillDone", store.resetForwardBackfillDone(ctx, "gid:g1"))

		store.saveAttachmentCacheEntry(ctx, "attrec1", []byte(`{"a":1}`))
		store.saveAttachmentCacheEntry(ctx, "attrec1", []byte(`{"a":2}`))
		cache, err := store.loadAttachmentCacheJSON(ctx)
		must("loadAttachmentCacheJSON", err)
		if len(cache["attrec1"]) == 0 {
			t.Errorf("attachment cache entry not persisted: %v", cache)
		}
		store.saveAttachmentCacheEntry(ctx, "orphan", []byte(`{}`))
		pruned, err := store.pruneOrphanedAttachmentCache(ctx)
		must("pruneOrphanedAttachmentCache", err)
		cache, err = store.loadAttachmentCacheJSON(ctx)
		must("loadAttachmentCacheJSON", err)
		if pruned != 1 || len(cache["attrec1"]) == 0 || cache["orphan"] != nil {
			t.Errorf("pruneOrphanedAttachmentCache pruned %d, cache now %v; want only the orphan removed", pruned, cache)
		}

		must("deleteLocalChatByPortalID", store.deleteLocalChatByPortalID(ctx, "tel:+1555"))
		_, err = store.listSoftDeletedPortals(ctx)
		must("listSoftDeletedPortals", err)
		must("undeleteCloudChatByPortalID", store.undeleteCloudChatByPortalID(ctx, "tel:+1555"))
		_, err = store.undeleteCloudMessagesByPortalID(ctx, "tel:+1555")
		must("undeleteCloudMessagesByPortalID", err)
		store.seedChatFromRecycleBin(ctx, "tel:+1999", "", "", "", "", []string{"tel:+1999"})
		if ok, err := store.portalHasChat(ctx, "tel:+1999"); err != nil || !ok {
			t.Errorf("seedChatFromRecycleBin did not insert: %v, %v", ok, err)
		}
		must("deleteMessageBatch", store.deleteMessageBatch(ctx, []string{"d1"}))
		must("deleteMessagesByChatIDs", store.deleteMessagesByChatIDs(ctx, []string{"chatD"}))
		must("deleteChatsByRecordNames", store.deleteChatsByRecordNames(ctx, []string{"crD"}))
		must("deleteChatBatch", store.deleteChatBatch(ctx, []string{"chatD"}))
		_, err = store.hardDeleteMessagesByPortalID(ctx, "tel:+1555")
		must("hardDeleteMessagesByPortalID", err)
		must("deleteLocalChatByGroupID", store.deleteLocalChatByGroupID(ctx, "g1"))
		_, err = store.deleteOrphanedMessages(ctx)
		must("deleteOrphanedMessages", err)
		must("clearZoneToken", store.clearZoneToken(ctx, cloudZoneMessages))
		must("clearSyncTokens", store.clearSyncTokens(ctx))
		must("clearAllData", store.clearAllData(ctx))
	})
}

func TestStoreSharedProfilesAndPendingAttachments(t *testing.T) {
	forEachTestDB(t, func(t *testing.T, db *dbutil.Database) {
		ctx := context.Background()
		profiles := newSharedProfileStore(db, testStoreLoginA)
		if err := profiles.ensureSchema(ctx); err != nil {
			t.Fatalf("sharedProfileStore.ensureSchema: %v", err)
		}
		row := &sharedProfileRow{Identifier: "tel:+1555", DisplayName: "A", RecordKey: "rk", DecryptionKey: []byte{0, 0xff}, Avatar: []byte{1, 2}, HasPoster: true, UpdatedTS: 1}
		if err := profiles.save(ctx, row); err != nil {
			t.Fatalf("save: %v", err)
		}
		row.DisplayName = "B"
		if err := profiles.save(ctx, row); err != nil {
			t.Fatalf("save (update): %v", err)
		}
		all, err := profiles.loadAll(ctx)
		if err != nil {
			t.Fatalf("loadAll: %v", err)
		}
		if len(all) != 1 || all[0].DisplayName != "B" || !all[0].HasPoster || len(all[0].DecryptionKey) != 2 {
			t.Errorf("loadAll = %+v", all)
		}

		pending := newPendingAttachmentStore(db, testStoreLoginA)
		if err := pending.ensureSchema(ctx); err != nil {
			t.Fatalf("pendingAttachmentStore.ensureSchema: %v", err)
		}
		now := time.Now().Truncate(time.Millisecond)
		p := &pendingAttachmentRow{MessageGUID: "m1", AttIndex: 0, AttID: "m1", PortalID: "p1", CreatedAt: now, NextAttemptAt: now}
		if err := pending.Insert(ctx, p); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if err := pending.Insert(ctx, p); err != nil {
			t.Fatalf("Insert (duplicate): %v", err)
		}
		due, err := pending.GetDue(ctx, now, 10)
		if err != nil || len(due) != 1 {
			t.Fatalf("GetDue = %d rows, %v", len(due), err)
		}
		p.AttemptCount = 1
		p.LastAttemptAt = now
		p.NextAttemptAt = now.Add(time.Second)
		if err := pending.UpdateSchedule(ctx, p); err != nil {
			t.Fatalf("UpdateSchedule: %v", err)
		}
		if one, err := pending.GetOne(ctx, "m1", 0); err != nil || one == nil || one.AttemptCount != 1 {
			t.Errorf("GetOne = %+v, %v", one, err)
		}
		if err := pending.Delete(ctx, p); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if one, err := pending.GetOne(ctx, "m1", 0); err != nil || one != nil {
			t.Errorf("GetOne after delete = %+v, %v", one, err)
		}
	})
}
