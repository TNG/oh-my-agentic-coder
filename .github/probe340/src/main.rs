fn main() {
    let url = std::env::args().nth(1).unwrap();
    match reqwest::blocking::get(&url) {
        Ok(r) => println!("GET {url}: {}", r.status()),
        Err(e) => { println!("GET {url}: ERR {e:?}"); std::process::exit(1) }
    }
}
