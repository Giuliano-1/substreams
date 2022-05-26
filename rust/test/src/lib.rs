mod pb;
use std::convert::TryInto;
use bigdecimal::BigDecimal;
use substreams::{log, Hex, errors::{SubstreamError}, store, store::{SumInt64Writer, MaxBigFloatWriter}};
use num_bigint::{BigInt, BigUint, TryFromBigIntError};
use hex_literal::hex;
use pb::{erc721, eth};

#[substreams::handlers::map]
fn map_transfers(blk: eth::Block) -> Result<erc721::Transfers, SubstreamError> {
    let mut transfers: Vec<erc721::Transfer> = vec![];

    for trx in blk.transaction_traces {
        transfers.extend(trx.receipt.as_ref().unwrap().logs.iter().filter_map(|log| {
            if log.address != TRACKED_CONTRACT {
                return None;
            }

            log::debug!("NFT Contract {} invoked", Hex(&TRACKED_CONTRACT));

            if !is_erc721transfer_event(log) {
                return None;
            }

            let token_id: Result<u64, TryFromBigIntError<BigUint>> =
                BigUint::from_bytes_be(&log.topics[3]).try_into();

            match token_id {
                Ok(token_id) => Some(erc721::Transfer {
                    trx_hash: trx.hash.clone(),
                    from: Vec::from(&log.topics[1][12..]),
                    to: Vec::from(&log.topics[2][12..]),
                    token_id,
                    ordinal: log.block_index as u64,
                }),
                Err(e) => {
                    log::info!(
                        "The token_id value {} does not fit in a 64 bits unsigned integer: {}",
                        Hex(&log.topics[3]),
                        e
                    );

                    None
                }
            }
        }));
    }
    return Ok(erc721::Transfers { transfers })
}

#[substreams::handlers::store]
fn build_nft_state(transfers: erc721::Transfers, s: store::SumInt64Writer, pairs: store::Reader, tokens: store::Reader) {
    let tokens_first_opt = tokens.get_first(&"tokens".to_owned());
    let pairs_last_opt = pairs.get_first(&"pairs".to_owned());
    log::info!("tokens {:?} pairs {:?}", tokens_first_opt, pairs_last_opt);
    for transfer in transfers.transfers {
        if hex::encode(&transfer.from) != "0000000000000000000000000000000000000000" {
            log::info!("found a transfer");
            s.sum(
                transfer.ordinal as i64,
                generate_key(transfer.from.as_ref()),
                -1,
            );
        }
        if hex::encode(&transfer.to) != "0000000000000000000000000000000000000000" {
            s.sum(
                transfer.ordinal as i64,
                generate_key(transfer.to.as_ref()),
                1,
            );
        }
    }
}

fn generate_key(holder: &[u8]) -> String {
    return format!(
        "total:{}:{}",
        Hex::encode(holder),
        Hex::encode(&TRACKED_CONTRACT)
    );
}

const TRACKED_CONTRACT: [u8; 20] = hex!("bc4ca0eda7647a8ab7c2061c2e118a18a936f13d");
/// keccak value for Transfer(address,address,uint256)
const TRANSFER_TOPIC: [u8; 32] = hex!("ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef");
pub fn is_erc721transfer_event(log: &eth::Log) -> bool {
    if log.topics.len() != 4 || log.data.len() != 0 {
        return false;
    }

    return log.topics[0] == TRANSFER_TOPIC;
}


#[substreams::handlers::store]
fn test_sum_big_int(s: store::SumBigIntWriter) {
    s.sum(
        1,
        "test.key.1".to_string(),
        &BigInt::parse_bytes(b"10", 10).unwrap(),
    );
    s.sum(
        1,
        "test.key.1".to_string(),
        &BigInt::parse_bytes(b"10", 10).unwrap(),
    );
}

#[substreams::handlers::store]
fn test_sum_int64(s: SumInt64Writer) {
    s.sum(1, "sum.int.64".to_string(), 10);
    s.sum(1, "sum.int.64".to_string(), 10);
}

#[substreams::handlers::store]
fn test_sum_float64(s: store::SumFloat64Writer) {
    s.sum(1, "sum.float.64".to_string(), 10.75);
    s.sum(1, "sum.float.64".to_string(), 10.75);
}

#[substreams::handlers::store]
fn test_sum_big_float_small_number(s: store::SumBigFloatWriter) {
    s.sum(
        1,
        "sum.big.float".to_string(),
        &BigDecimal::parse_bytes(b"10.5", 10).unwrap(),
    );
    s.sum(
        1,
        "sum.big.float".to_string(),
        &BigDecimal::parse_bytes(b"10.5", 10).unwrap(),
    );
}

#[substreams::handlers::store]
fn test_sum_big_float_big_number(s: store::SumBigFloatWriter) {
    s.sum(
        1,
        "sum.big.float".to_string(),
        &BigDecimal::parse_bytes(b"12345678987654321.5", 10).unwrap(),
    );
    s.sum(
        1,
        "sum.big.float".to_string(),
        &BigDecimal::parse_bytes(b"12345678987654321.5", 10).unwrap(),
    );
}

#[substreams::handlers::store]
fn test_set_min_int64(s: store::MinInt64Writer) {
    s.min(1, "set_min_int64".to_string(), 5);
    s.min(1, "set_min_int64".to_string(), 2);
}

#[substreams::handlers::store]
fn test_set_min_bigint(s: store::MinBigIntWriter) {
    s.min(
        1,
        "set_min_bigint".to_string(),
        &BigInt::parse_bytes(b"5", 10).unwrap(),
    );
    s.min(
        1,
        "set_min_bigint".to_string(),
        &BigInt::parse_bytes(b"3", 10).unwrap(),
    );
}

#[substreams::handlers::store]
fn test_set_min_float64(s: store::MinFloat64Writer) {
    s.min(1, "set_min_float64".to_string(), 10.05);
    s.min(1, "set_min_float64".to_string(), 10.04);
}

#[substreams::handlers::store]
fn test_set_min_bigfloat(s: store::MinBigFloatWriter) {
    s.min(
        1,
        "set_min_bigfloat".to_string(),
        &BigDecimal::parse_bytes(b"11.05", 10).unwrap(),
    );
    s.min(
        1,
        "set_min_bigfloat".to_string(),
        &BigDecimal::parse_bytes(b"11.04", 10).unwrap(),
    );
}

#[substreams::handlers::store]
fn test_set_max_int64(s: store::MaxInt64Writer) {
    s.max(1, "set_max_int64".to_string(), 5);
    s.max(1, "set_max_int64".to_string(), 2);
}

#[substreams::handlers::store]
fn test_set_max_bigint(my_store: store::MaxBigIntWriter) {
    my_store.max(
        1,
        "set_max_bigint".to_string(),
        &BigInt::parse_bytes(b"5", 10).unwrap(),
    );
    my_store.max(
        1,
        "set_max_bigint".to_string(),
        &BigInt::parse_bytes(b"3", 10).unwrap(),
    );
}

#[substreams::handlers::store]
fn test_set_max_float64(s: store::MaxFloat64Writer) {
    s.max(1, "set_max_float64".to_string(), 10.05);
    s.max(1, "set_max_float64".to_string(), 10.04);
}

#[substreams::handlers::store]
fn test_set_max_bigfloat(s: MaxBigFloatWriter) {
    s.max(
        1,
        "set_max_bigfloat".to_string(),
        &BigDecimal::parse_bytes(b"11.05", 10).unwrap(),
    );
    s.max(
        1,
        "set_max_bigfloat".to_string(),
        &BigDecimal::parse_bytes(b"11.04", 10).unwrap(),
    );
}

// wasm extension tests
#[link(wasm_import_module = "myext")]
extern "C" {
    pub fn myimport(rpc_call_offset: *const u8, rpc_call_len: u32, rpc_response_ptr: *const u8);
}

pub fn do_myimport(input: Vec<u8>) -> Vec<u8> {
    unsafe {
        let response_ptr = substreams::memory::alloc(8);
        myimport(input.as_ptr(), input.len() as u32, response_ptr);
        return substreams::memory::get_output_data(response_ptr);
    }
}

#[substreams::handlers::store]
fn test_wasm_extension_hello() {
    substreams::log::println("first".to_string());

    do_myimport(Vec::from("hello"));
    // Print a certain log statement if val == "world"
    // Print a different one if `do_myimport` failed, or will it even come back?
    substreams::log::println("second".to_string());
}

#[substreams::handlers::store]
fn test_wasm_extension_fail() {
    substreams::log::println("first".to_string());

    do_myimport(Vec::from("failfast"));
    // Print a certain log statement if val == "world"
    // Print a different one if `do_myimport` failed, or will it even come back?

    substreams::log::println("second".to_string());
}

#[substreams::handlers::store]
extern "C" fn test_set_delete_prefix(s: store::UpdateWriter) {
    s.set(1, "1:key_to_keep".to_string(), &[1, 2, 3, 4].to_vec());
    s.set(2, "2:key_to_delete".to_string(), &[5, 6, 7, 8].to_vec());
    s.delete_prefix(3, &"2:".to_string());
}

#[no_mangle]
extern "C" fn test_make_it_crash(data_ptr: *mut u8, data_len: usize) {
    unsafe {
        let input_data = Vec::from_raw_parts(data_ptr, data_len, data_len);
        let cloned_data = input_data.clone();
        substreams::output_raw(cloned_data);
    };
}

// #[no_mangle]
// extern "C" fn test_memory_leak() {
//     substreams::memory::alloc(10485760); // allocate 1MB on each call
// }
