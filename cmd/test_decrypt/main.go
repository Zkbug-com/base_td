package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/crypto"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/pbkdf2"
)

func main() {
	// 从环境变量获取配置
	masterKeyStr := os.Getenv("GENERATOR_MASTER_KEY")
	if masterKeyStr == "" {
		fmt.Println("❌ 请设置 GENERATOR_MASTER_KEY 环境变量")
		os.Exit(1)
	}

	// 注意: Rust 直接使用字符串字节 (args.master_key.as_bytes())
	// 所以 Go 也要用字符串字节，而不是 hex.DecodeString
	masterKey := []byte(masterKeyStr)

	fmt.Printf("✅ Master key 长度: %d 字节 (字符串)\n", len(masterKey))

	// 连接数据库
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://poison_db:poison_db@123@localhost:5432/poison_db?sslmode=disable"
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		fmt.Printf("❌ 数据库连接失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("✅ 数据库连接成功")

	// 获取一条地址记录
	var address string
	var encryptedPK []byte
	err = db.QueryRow(`
		SELECT address, encrypted_private_key 
		FROM vanity_addresses 
		LIMIT 1
	`).Scan(&address, &encryptedPK)
	if err != nil {
		fmt.Printf("❌ 查询失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ 获取地址: 0x%s\n", address)
	fmt.Printf("   加密私钥长度: %d 字节\n", len(encryptedPK))

	// 解密私钥
	privateKey, err := decryptPrivateKey(encryptedPK, masterKey)
	if err != nil {
		fmt.Printf("❌ 解密失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ 解密成功，私钥长度: %d 字节\n", len(privateKey))

	// 验证私钥能否导出正确的地址
	ecdsaKey, err := crypto.ToECDSA(privateKey)
	if err != nil {
		fmt.Printf("❌ 私钥转换失败: %v\n", err)
		os.Exit(1)
	}

	derivedAddr := crypto.PubkeyToAddress(ecdsaKey.PublicKey)
	derivedAddrHex := hex.EncodeToString(derivedAddr.Bytes())

	fmt.Printf("   数据库地址: %s\n", address)
	fmt.Printf("   导出地址:   %s\n", derivedAddrHex)

	if derivedAddrHex == address {
		fmt.Println("✅ 地址验证成功！私钥解密正确")
	} else {
		fmt.Println("❌ 地址不匹配！解密可能有问题")
		os.Exit(1)
	}
}

func decryptPrivateKey(encrypted []byte, masterKey []byte) ([]byte, error) {
	if len(encrypted) != 60 {
		return nil, fmt.Errorf("invalid encrypted data length: %d, expected 60", len(encrypted))
	}

	// 派生密钥 (与Rust相同)
	derivedKey := pbkdf2.Key(masterKey, []byte("address-generator-salt"), 10000, 32, sha256.New)

	// 提取nonce和ciphertext
	nonce := encrypted[:12]
	ciphertext := encrypted[12:]

	// AES-256-GCM解密
	block, err := aes.NewCipher(derivedKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}
