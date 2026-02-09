//! CLI for iMessage login + IDS registration.
//!
//! Reuses the exact same login code as the bridge (rustpushgo lib).
//! Can be run directly on a Mac without the Beeper bot.
//!
//! Usage:
//!   cargo run --bin imessage-login
//!
//! Or with an external hardware key (requires hardware-key feature):
//!   cargo run --bin imessage-login --features hardware-key -- --hardware-key <base64>

use std::io::{self, Write};

use rustpushgo::{
    connect, create_local_macos_config, init_logger, login_start, WrappedAPSState, WrappedOSConfig,
    WrappedError,
};
use std::sync::Arc;

fn prompt(msg: &str) -> String {
    eprint!("{}", msg);
    io::stderr().flush().unwrap();
    let mut input = String::new();
    io::stdin().read_line(&mut input).unwrap();
    input.trim().to_string()
}

fn create_config(hw_key: Option<String>) -> Result<Arc<WrappedOSConfig>, WrappedError> {
    if let Some(key) = hw_key {
        #[cfg(feature = "hardware-key")]
        {
            eprintln!("[*] Using external hardware key");
            return rustpushgo::create_config_from_hardware_key(key);
        }
        #[cfg(not(feature = "hardware-key"))]
        {
            let _ = key;
            return Err(WrappedError::GenericError {
                msg: "hardware-key feature not enabled. Rebuild with --features hardware-key".into(),
            });
        }
    }
    eprintln!("[*] Using local macOS config (IOKit + AAAbsintheContext)");
    create_local_macos_config()
}

#[tokio::main]
async fn main() {
    init_logger();

    let args: Vec<String> = std::env::args().collect();
    let hw_key = args.iter().position(|a| a == "--hardware-key").map(|i| args[i + 1].clone());

    // --- Create config ---
    let cfg = create_config(hw_key).unwrap_or_else(|e| {
        eprintln!("[!] Config failed: {e}");
        std::process::exit(1);
    });

    eprintln!("[*] Device UUID: {}", cfg.get_device_id());

    // --- Connect APS ---
    eprintln!("[*] Connecting to APNs...");
    let conn = connect(&cfg, &WrappedAPSState::new(None)).await;
    eprintln!("[+] APNs connected");

    // --- Prompt for credentials ---
    let username = prompt("Apple ID: ");
    let password = prompt("Password: ");

    // --- Start login (same code path as bridge) ---
    eprintln!("[*] Starting login...");
    let session = match login_start(username, password, &cfg, &conn).await {
        Ok(s) => s,
        Err(e) => {
            eprintln!("[!] Login failed: {e}");
            std::process::exit(1);
        }
    };

    // --- 2FA if needed ---
    if session.needs_2fa() {
        eprintln!("[*] 2FA required — check your trusted devices");
        let code = prompt("2FA code: ");
        match session.submit_2fa(code).await {
            Ok(true) => eprintln!("[+] 2FA accepted"),
            Ok(false) => {
                eprintln!("[!] 2FA rejected — invalid code");
                std::process::exit(1);
            }
            Err(e) => {
                eprintln!("[!] 2FA error: {e}");
                std::process::exit(1);
            }
        }
    } else {
        eprintln!("[+] Login succeeded without 2FA");
    }

    // --- Finish: IDS auth + registration (same code path as bridge) ---
    eprintln!("[*] Authenticating with IDS and registering...");
    match session.finish(&cfg, &conn).await {
        Ok(result) => {
            eprintln!("[+] Registration successful!");
            eprintln!("[+] Login ID: {}", result.users.login_id(0));
            let handles = result.users.inner.iter()
                .flat_map(|u| u.registration.values())
                .flat_map(|r| r.handles.iter())
                .collect::<Vec<_>>();
            eprintln!("[+] Handles: {:?}", handles);
        }
        Err(e) => {
            eprintln!("[!] Registration FAILED: {e}");
            std::process::exit(1);
        }
    }
}
