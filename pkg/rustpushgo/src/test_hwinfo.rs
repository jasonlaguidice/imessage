#[cfg(test)]
mod tests {
    use crate::local_config::HardwareInfo;

    #[test]
    fn test_hardware_info_read() {
        let info = HardwareInfo::read().expect("Failed to read hardware info");
        eprintln!("Product: {}", info.product_name);
        eprintln!("Serial: {}", info.serial_number);
        eprintln!("UUID: {}", info.platform_uuid);
        eprintln!("Build: {}", info.os_build_num);
        eprintln!("Version: {}", info.os_version);
        eprintln!("ROM: {} bytes", info.rom.len());
        eprintln!("MLB: {}", info.mlb);
        eprintln!("MAC: {:02x?}", info.mac_address);

        assert!(!info.product_name.is_empty(), "product name should not be empty");
        assert!(!info.serial_number.is_empty(), "serial number should not be empty");
        assert!(!info.os_version.is_empty(), "os version should not be empty");
    }
}
