/**
 * validation_data.h — C FFI interface for generating Apple APNs validation data
 *
 * Link with: -framework Foundation -fobjc-arc
 */

#ifndef VALIDATION_DATA_H
#define VALIDATION_DATA_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

/**
 * Generate APNs validation data for IDS registration.
 *
 * This function handles the entire NAC protocol:
 *   1. Fetches the validation certificate from Apple
 *   2. Initializes a NAC context (NACInit)
 *   3. Sends session info request to Apple (HTTP POST)
 *   4. Performs key establishment (NACKeyEstablishment)
 *   5. Signs and produces validation data (NACSign)
 *
 * The hardware identifiers are read automatically from IOKit.
 *
 * @param out_buf      On success, receives a malloc'd buffer with validation data.
 *                     Caller must free() this buffer.
 * @param out_len      On success, receives the length of the validation data.
 * @param out_err_buf  On failure, receives a malloc'd error message string.
 *                     Caller must free() this buffer. May be NULL.
 * @return 0 on success, non-zero error code on failure.
 *
 * Error codes:
 *   1  = Failed to load AppleAccount.framework
 *   2  = Failed to fetch validation certificate
 *   3  = Invalid certificate plist format
 *   4  = AAAbsintheContext class not found
 *   5  = NACInit failed
 *   6  = HTTP request to initializeValidation failed
 *   7  = Invalid response plist
 *   8  = Server returned non-zero status
 *   9  = No session-info in response
 *  10  = NACKeyEstablishment failed
 *  11  = NACSign failed
 */
int nac_generate_validation_data(uint8_t **out_buf, size_t *out_len, char **out_err_buf);

#ifdef __cplusplus
}
#endif

#endif /* VALIDATION_DATA_H */
