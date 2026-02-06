/**
 * hardware_info.m — Read Mac hardware identifiers from IOKit for iMessage registration.
 *
 * Provides: model, serial number, platform UUID, board ID, ROM, MLB, MAC address,
 * root disk UUID, OS build number, and OS version.
 */

#import <Foundation/Foundation.h>
#import <IOKit/IOKitLib.h>
#import <sys/sysctl.h>
#import <sys/mount.h>
#include "hardware_info.h"

// ---- IOKit helpers ----

static char *iokit_string(io_service_t service, CFStringRef key) {
    CFTypeRef ref = IORegistryEntryCreateCFProperty(service, key, kCFAllocatorDefault, 0);
    if (!ref) return NULL;
    char *result = NULL;
    if (CFGetTypeID(ref) == CFStringGetTypeID()) {
        const char *utf8 = CFStringGetCStringPtr(ref, kCFStringEncodingUTF8);
        if (utf8) {
            result = strdup(utf8);
        } else {
            char buf[256];
            if (CFStringGetCString(ref, buf, sizeof(buf), kCFStringEncodingUTF8)) {
                result = strdup(buf);
            }
        }
    }
    CFRelease(ref);
    return result;
}

static uint8_t *iokit_data(io_service_t service, CFStringRef key, size_t *out_len) {
    CFTypeRef ref = IORegistryEntryCreateCFProperty(service, key, kCFAllocatorDefault, 0);
    if (!ref) { *out_len = 0; return NULL; }
    uint8_t *result = NULL;
    if (CFGetTypeID(ref) == CFDataGetTypeID()) {
        CFDataRef data = (CFDataRef)ref;
        CFIndex len = CFDataGetLength(data);
        result = (uint8_t *)malloc(len);
        CFDataGetBytes(data, CFRangeMake(0, len), result);
        *out_len = (size_t)len;
    } else {
        *out_len = 0;
    }
    CFRelease(ref);
    return result;
}

// ---- Main ----

HardwareInfo hw_info_read(void) {
    HardwareInfo info = {0};

    // --- Platform expert (serial, UUID, model, board ID) ---
    io_service_t platformExpert = IOServiceGetMatchingService(
        kIOMainPortDefault, IOServiceMatching("IOPlatformExpertDevice"));

    if (!platformExpert) {
        info.error = strdup("Failed to find IOPlatformExpertDevice");
        return info;
    }

    info.serial_number = iokit_string(platformExpert, CFSTR("IOPlatformSerialNumber"));
    info.platform_uuid = iokit_string(platformExpert, CFSTR("IOPlatformUUID"));
    info.board_id = iokit_string(platformExpert, CFSTR("board-id"));
    info.product_name = iokit_string(platformExpert, CFSTR("model"));

    // Fallback for product_name via sysctl
    if (!info.product_name) {
        char model[64] = {0};
        size_t len = sizeof(model);
        if (sysctlbyname("hw.model", model, &len, NULL, 0) == 0) {
            info.product_name = strdup(model);
        }
    }

    // --- MLB (Main Logic Board serial) ---
    // On Apple Silicon, mlb-serial-number is on the platform expert as raw data with trailing NULLs
    {
        size_t mlbDataLen = 0;
        uint8_t *mlbData = iokit_data(platformExpert, CFSTR("mlb-serial-number"), &mlbDataLen);
        if (mlbData && mlbDataLen > 0) {
            // Strip trailing NUL bytes
            size_t realLen = mlbDataLen;
            while (realLen > 0 && mlbData[realLen - 1] == 0) realLen--;
            if (realLen > 0) {
                info.mlb = strndup((char *)mlbData, realLen);
            }
            free(mlbData);
        }
    }

    IOObjectRelease(platformExpert);

    // --- EFI ROM ---
    // On Intel: IODeviceTree:/options. On Apple Silicon: may not exist.
    // Try NVRAM first, fall back to MAC address as ROM (common for AS Macs).
    io_registry_entry_t options = IORegistryEntryFromPath(
        kIOMainPortDefault, "IODeviceTree:/options");

    if (options != MACH_PORT_NULL) {
        info.rom = iokit_data(options, CFSTR("4D1EDE05-38C7-4A6A-9CC6-4BCCA8B38C14:ROM"), &info.rom_len);
        if (!info.rom || info.rom_len == 0) {
            info.rom = iokit_data(options, CFSTR("ROM"), &info.rom_len);
        }
        if (!info.mlb) {
            info.mlb = iokit_string(options, CFSTR("4D1EDE05-38C7-4A6A-9CC6-4BCCA8B38C14:MLB"));
        }
        if (!info.mlb) {
            info.mlb = iokit_string(options, CFSTR("MLB"));
        }
        IOObjectRelease(options);
    }

    // --- MAC address (en0) ---
    CFMutableDictionaryRef matchDict = IOServiceMatching("IOEthernetInterface");
    io_iterator_t iterator;
    if (IOServiceGetMatchingServices(kIOMainPortDefault, matchDict, &iterator) == KERN_SUCCESS) {
        io_service_t service;
        while ((service = IOIteratorNext(iterator)) != 0) {
            // Check if this is en0 (primary)
            CFTypeRef bsdName = IORegistryEntryCreateCFProperty(service, CFSTR("BSD Name"), kCFAllocatorDefault, 0);
            BOOL isPrimary = NO;
            if (bsdName && CFGetTypeID(bsdName) == CFStringGetTypeID()) {
                isPrimary = CFStringCompare(bsdName, CFSTR("en0"), 0) == kCFCompareEqualTo;
            }
            if (bsdName) CFRelease(bsdName);

            if (isPrimary) {
                // Get MAC from parent (IOEthernetController)
                io_service_t parent;
                if (IORegistryEntryGetParentEntry(service, kIOServicePlane, &parent) == KERN_SUCCESS) {
                    info.mac_address = iokit_data(parent, CFSTR("IOMACAddress"), &info.mac_address_len);
                    IOObjectRelease(parent);
                }
                IOObjectRelease(service);
                break;
            }
            IOObjectRelease(service);
        }
        IOObjectRelease(iterator);
    }

    // --- ROM fallback for Apple Silicon: use MAC address ---
    if ((!info.rom || info.rom_len == 0) && info.mac_address && info.mac_address_len == 6) {
        info.rom_len = 6;
        info.rom = (uint8_t *)malloc(6);
        memcpy(info.rom, info.mac_address, 6);
    }

    // --- Root disk UUID ---
    // Use DiskArbitration or IOKit. Simpler: parse from `diskutil info /` or IOKit.
    // For now, read from IOKit APFS container
    io_service_t mediaService = IOServiceGetMatchingService(
        kIOMainPortDefault, IOServiceMatching("IOMediaBSDClient"));
    if (mediaService) {
        // This doesn't directly give us the root UUID. Use a different approach.
        IOObjectRelease(mediaService);
    }
    // Fallback: use a fixed approach with sysctl or getfsstat
    {
        struct statfs sfs;
        if (statfs("/", &sfs) == 0) {
            // sfs.f_mntfromname is like "/dev/disk3s1s1"
            // Get UUID via DADiskCreateFromBSDName... but that requires DiskArbitration.
            // Simpler: use IOKit to find the matching media
            char *bsdDisk = sfs.f_mntfromname;
            if (strncmp(bsdDisk, "/dev/", 5) == 0) bsdDisk += 5;

            // Strip trailing snapshot suffixes for APFS
            char diskName[64];
            strncpy(diskName, bsdDisk, sizeof(diskName) - 1);
            diskName[sizeof(diskName) - 1] = '\0';
            // Remove trailing 's' suffixes (e.g., disk3s1s1 → disk3s1)
            // Actually we want the volume group UUID, not the partition
            // For registration purposes, use the platform UUID as fallback
        }
        // Use platform UUID as root disk UUID fallback (common in Apple registration)
        if (!info.root_disk_uuid && info.platform_uuid) {
            info.root_disk_uuid = strdup(info.platform_uuid);
        }
    }

    // --- OS build number and version ---
    {
        char build[32] = {0};
        size_t len = sizeof(build);
        if (sysctlbyname("kern.osversion", build, &len, NULL, 0) == 0) {
            info.os_build_num = strdup(build);
        }
    }
    // Get macOS version from NSProcessInfo
    {
        NSOperatingSystemVersion ver = [[NSProcessInfo processInfo] operatingSystemVersion];
        NSString *verStr = [NSString stringWithFormat:@"%ld.%ld",
            (long)ver.majorVersion, (long)ver.minorVersion];
        if (ver.patchVersion > 0) {
            verStr = [NSString stringWithFormat:@"%@.%ld", verStr, (long)ver.patchVersion];
        }
        info.os_version = strdup([verStr UTF8String]);
    }

    return info;
}

void hw_info_free(HardwareInfo *info) {
    if (!info) return;
    free(info->product_name);
    free(info->serial_number);
    free(info->platform_uuid);
    free(info->board_id);
    free(info->os_build_num);
    free(info->os_version);
    free(info->rom);
    free(info->mlb);
    free(info->mac_address);
    free(info->root_disk_uuid);
    free(info->error);
    memset(info, 0, sizeof(HardwareInfo));
}
