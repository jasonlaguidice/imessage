use std::env;
use std::process::Command;

fn main() {
    // Declare the custom cfg so cargo doesn't warn about it
    println!("cargo:rustc-check-cfg=cfg(has_xnu_encrypt)");

    let target = env::var("TARGET").unwrap_or_default();

    if target.starts_with("x86_64") && target.contains("linux") {
        // Native path: assemble encrypt.s and link as a static library.
        let out_dir = env::var("OUT_DIR").unwrap();
        let asm_src = "src/asm/encrypt.s";
        let obj_path = format!("{}/encrypt.o", out_dir);
        let lib_path = format!("{}/libxnu_encrypt.a", out_dir);

        let status = Command::new("cc")
            .args(["-c", "-o", &obj_path, asm_src])
            .status()
            .expect("Failed to run assembler on encrypt.s");
        assert!(status.success(), "Assembly of encrypt.s failed");

        let status = Command::new("ar")
            .args(["rcs", &lib_path, &obj_path])
            .status()
            .expect("Failed to run ar");
        assert!(status.success(), "ar failed to create libxnu_encrypt.a");

        println!("cargo:rustc-link-search=native={}", out_dir);
        println!("cargo:rustc-link-lib=static=xnu_encrypt");
        println!("cargo:rerun-if-changed={}", asm_src);
        println!("cargo:rustc-cfg=has_xnu_encrypt");
    } else if target.starts_with("aarch64") && target.contains("linux") {
        // arm64 path: pre-compiled x86_64 binary blobs (checked into src/asm/) are
        // loaded at runtime and executed through the unicorn x86-64 emulator.
        // No native assembly compilation needed here.
        println!("cargo:rerun-if-changed=src/asm/encrypt_x86_64_text.bin");
        println!("cargo:rerun-if-changed=src/asm/encrypt_x86_64_data.bin");
        println!("cargo:rustc-cfg=has_xnu_encrypt");
    } else {
        return;
    }
}
