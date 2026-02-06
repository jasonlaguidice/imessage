//! Apple APNs validation data generation for macOS 14.2+ (Sonoma or later)
//!
//! This crate provides `generate_validation_data()` which calls the NAC
//! (Network Attestation Credential) functions via Apple's private
//! `AppleAccount.framework` (class `AAAbsintheContext`) to produce the
//! opaque validation data blob required for IDS registration.
//!
//! # Requirements
//! - macOS 14.2+ (Sonoma or later)
//! - SIP can remain enabled
//! - No jailbreak or code injection required
//! - Network access to Apple's servers
//!
//! # Build
//! The `validation_data.m` Objective-C file must be compiled and linked.
//! Use the provided `build.rs` or compile manually:
//! ```sh
//! cc -c validation_data.m -framework Foundation -fobjc-arc -o validation_data.o
//! ```

use std::ffi::CStr;
use std::ptr;

extern "C" {
    fn nac_generate_validation_data(
        out_buf: *mut *mut u8,
        out_len: *mut usize,
        out_err_buf: *mut *mut std::os::raw::c_char,
    ) -> i32;
}

/// Error type for validation data generation.
#[derive(Debug, thiserror::Error)]
pub enum NacError {
    #[error("NAC error (code {code}): {message}")]
    NacFailed { code: i32, message: String },
}

/// Generate APNs validation data for IDS registration.
///
/// This handles the full NAC protocol:
/// 1. Fetches validation certificate from Apple
/// 2. NACInit with the certificate
/// 3. Sends session info request to Apple's servers
/// 4. NACKeyEstablishment with the response
/// 5. NACSign to produce the final validation data
///
/// Hardware identifiers are read automatically from IOKit.
///
/// Returns the raw validation data bytes on success.
pub fn generate_validation_data() -> Result<Vec<u8>, NacError> {
    let mut buf: *mut u8 = ptr::null_mut();
    let mut len: usize = 0;
    let mut err_buf: *mut std::os::raw::c_char = ptr::null_mut();

    let result = unsafe { nac_generate_validation_data(&mut buf, &mut len, &mut err_buf) };

    if result == 0 && !buf.is_null() {
        let data = unsafe { std::slice::from_raw_parts(buf, len) }.to_vec();
        unsafe { libc::free(buf as *mut _) };
        Ok(data)
    } else {
        let message = if !err_buf.is_null() {
            let msg = unsafe { CStr::from_ptr(err_buf) }
                .to_string_lossy()
                .into_owned();
            unsafe { libc::free(err_buf as *mut _) };
            msg
        } else {
            format!("Unknown NAC error (code {})", result)
        };

        Err(NacError::NacFailed {
            code: result,
            message,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_generate_validation_data() {
        let data = generate_validation_data().expect("Failed to generate validation data");
        assert!(!data.is_empty(), "Validation data should not be empty");
        assert!(
            data.len() > 100,
            "Validation data should be substantial (got {} bytes)",
            data.len()
        );
        eprintln!(
            "Generated {} bytes of validation data",
            data.len()
        );
    }
}
