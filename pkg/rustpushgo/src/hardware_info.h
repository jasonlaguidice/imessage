#ifndef HARDWARE_INFO_H
#define HARDWARE_INFO_H

#include <stdint.h>
#include <stddef.h>

typedef struct {
    char *product_name;         // e.g., "Mac15,3"
    char *serial_number;        // e.g., "C02XX..."
    char *platform_uuid;        // hardware UUID
    char *board_id;             // e.g., "Mac-..."
    char *os_build_num;         // e.g., "25B78"
    char *os_version;           // e.g., "26.1"
    uint8_t *rom;               // EFI ROM
    size_t rom_len;
    char *mlb;                  // Main Logic Board serial
    uint8_t *mac_address;       // 6-byte MAC
    size_t mac_address_len;
    char *root_disk_uuid;       // root volume UUID
    char *error;                // set on failure
} HardwareInfo;

/// Read hardware identifiers from IOKit/sysctl. Caller must call hw_info_free().
HardwareInfo hw_info_read(void);

/// Free a HardwareInfo's allocated strings/buffers.
void hw_info_free(HardwareInfo *info);

#endif
