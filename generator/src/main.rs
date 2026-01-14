use aes_gcm::{aead::Aead, Aes256Gcm, KeyInit, Nonce};
use clap::Parser;
use deadpool_postgres::{Config, Pool, Runtime};
use pbkdf2::pbkdf2_hmac;
use rayon::prelude::*;
use secp256k1::{rand::rngs::OsRng, Secp256k1};
use sha2::Sha256;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Instant;
use tiny_keccak::{Hasher, Keccak};
use tokio_postgres::NoTls;
use tracing::{info, warn};

#[derive(Parser, Debug)]
#[command(author, version, about)]
struct Args {
    /// 并行线程数
    #[arg(short, long, default_value_t = num_cpus::get())]
    threads: usize,
    /// 每批写入数据库的数量
    #[arg(short, long, default_value_t = 1000)]
    batch_size: usize,
    /// PostgreSQL连接字符串
    #[arg(long, env = "DATABASE_URL")]
    database_url: String,
    /// 加密主密钥 (至少32字节)
    #[arg(long, env = "MASTER_KEY")]
    master_key: String,
    /// 目标表名 (默认 vanity_addresses，轮换时用 vanity_addresses_b)
    #[arg(long, default_value = "vanity_addresses")]
    table: String,
}

/// 生成的地址信息
#[derive(Clone)]
struct VanityAddress {
    address: String,     // 40位hex
    prefix: String,      // 前4位
    prefix3: String,     // 前3位 (用于模糊匹配)
    suffix: String,      // 后4位
    encrypted_pk: Vec<u8>, // 加密后的私钥
}

/// 加密私钥
fn encrypt_private_key(private_key: &[u8], master_key: &[u8]) -> Vec<u8> {
    // 派生加密密钥
    let mut derived_key = [0u8; 32];
    pbkdf2_hmac::<Sha256>(master_key, b"address-generator-salt", 10000, &mut derived_key);
    
    // AES-256-GCM加密
    let cipher = Aes256Gcm::new_from_slice(&derived_key).unwrap();
    let nonce_bytes: [u8; 12] = rand::random();
    let nonce = Nonce::from_slice(&nonce_bytes);
    
    let ciphertext = cipher.encrypt(nonce, private_key).unwrap();
    
    // 返回 nonce + ciphertext
    let mut result = nonce_bytes.to_vec();
    result.extend(ciphertext);
    result
}

/// 生成单个以太坊地址
fn generate_address(master_key: &[u8]) -> VanityAddress {
    let secp = Secp256k1::new();
    let (secret_key, public_key) = secp.generate_keypair(&mut OsRng);
    
    // 获取未压缩公钥 (65字节，去掉第一个字节0x04)
    let pk_bytes = public_key.serialize_uncompressed();
    
    // Keccak256哈希
    let mut hasher = Keccak::v256();
    let mut hash = [0u8; 32];
    hasher.update(&pk_bytes[1..]); // 跳过0x04前缀
    hasher.finalize(&mut hash);
    
    // 取后20字节作为地址
    let address = hex::encode(&hash[12..]);
    let prefix = address[..4].to_string();
    let prefix3 = address[..3].to_string();
    let suffix = address[36..].to_string();

    // 加密私钥
    let encrypted_pk = encrypt_private_key(&secret_key.secret_bytes(), master_key);

    VanityAddress {
        address,
        prefix,
        prefix3,
        suffix,
        encrypted_pk,
    }
}

/// 批量生成地址
fn generate_batch(count: usize, master_key: &[u8]) -> Vec<VanityAddress> {
    (0..count)
        .into_par_iter()
        .map(|_| generate_address(master_key))
        .collect()
}

/// 批量写入数据库
async fn insert_batch(pool: &Pool, addresses: Vec<VanityAddress>, table_name: &str) -> Result<u64, Box<dyn std::error::Error>> {
    let client = pool.get().await?;

    // 批量插入 (包含prefix3字段)
    let sql = format!(
        "INSERT INTO {} (address, prefix, prefix3, suffix, encrypted_private_key) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (address) DO NOTHING",
        table_name
    );
    let stmt = client.prepare(&sql).await?;

    let mut inserted = 0u64;
    for addr in addresses {
        let result = client
            .execute(&stmt, &[&addr.address, &addr.prefix, &addr.prefix3, &addr.suffix, &addr.encrypted_pk])
            .await?;
        inserted += result;
    }

    Ok(inserted)
}

/// 创建数据库连接池
async fn create_pool(database_url: &str) -> Result<Pool, Box<dyn std::error::Error>> {
    let mut cfg = Config::new();
    cfg.url = Some(database_url.to_string());
    cfg.pool = Some(deadpool_postgres::PoolConfig::new(30));
    
    let pool = cfg.create_pool(Some(Runtime::Tokio1), NoTls)?;
    Ok(pool)
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    // 初始化日志
    tracing_subscriber::fmt::init();
    
    let args = Args::parse();
    
    // 验证主密钥长度
    if args.master_key.len() < 32 {
        panic!("Master key must be at least 32 bytes");
    }
    
    info!("Starting address generator with {} threads", args.threads);
    
    // 设置rayon线程池
    rayon::ThreadPoolBuilder::new()
        .num_threads(args.threads)
        .build_global()?;
    
    // 创建数据库连接池
    let pool = create_pool(&args.database_url).await?;
    info!("Database connection pool created");
    info!("Target table: {}", args.table);

    let master_key = args.master_key.as_bytes();
    let table_name = args.table.clone();
    let total_generated = Arc::new(AtomicU64::new(0));
    let total_inserted = Arc::new(AtomicU64::new(0));
    let start_time = Instant::now();

    // 主循环：持续生成地址
    loop {
        let batch = generate_batch(args.batch_size, master_key);
        let count = batch.len() as u64;
        total_generated.fetch_add(count, Ordering::Relaxed);

        match insert_batch(&pool, batch, &table_name).await {
            Ok(inserted) => {
                total_inserted.fetch_add(inserted, Ordering::Relaxed);
            }
            Err(e) => {
                warn!("Database insert error: {}", e);
            }
        }

        // 每10秒打印统计
        let elapsed = start_time.elapsed().as_secs();
        if elapsed > 0 && elapsed % 10 == 0 {
            let gen = total_generated.load(Ordering::Relaxed);
            let ins = total_inserted.load(Ordering::Relaxed);
            info!(
                "Generated: {}, Inserted: {}, Rate: {}/s",
                gen, ins, gen / elapsed
            );
        }
    }
}

