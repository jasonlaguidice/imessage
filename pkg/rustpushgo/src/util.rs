use std::io::Cursor;

pub fn plist_to_buf<T: serde::Serialize + ?Sized>(value: &T) -> Result<Vec<u8>, plist::Error> {
    let mut buf: Vec<u8> = Vec::new();
    let writer = Cursor::new(&mut buf);
    plist::to_writer_xml(writer, &value)?;
    Ok(buf)
}

pub fn plist_to_string<T>(value: &T) -> Result<String, plist::Error>
where
    T: serde::Serialize + ?Sized,
{
    plist_to_buf(value).map(|val| String::from_utf8(val).unwrap())
}

pub fn plist_from_buf<T: serde::de::DeserializeOwned>(buf: &[u8]) -> Result<T, plist::Error> {
    let reader = Cursor::new(buf);
    plist::from_reader_xml(reader)
}

pub fn plist_from_string<T: serde::de::DeserializeOwned>(s: &str) -> Result<T, plist::Error> {
    plist_from_buf(s.as_bytes())
}
