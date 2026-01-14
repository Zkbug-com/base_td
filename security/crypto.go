package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/pbkdf2"
)

const (
	// AES-256需要32字节密钥
	KeySize = 32
	// GCM nonce大小
	NonceSize = 12
	// PBKDF2迭代次数
	PBKDF2Iterations = 100000
	// Salt大小
	SaltSize = 16
)

// Crypto AES-256-GCM加密器
type Crypto struct {
	key []byte
}

// NewCrypto 从密码创建加密器
func NewCrypto(password string, salt []byte) (*Crypto, error) {
	if len(salt) != SaltSize {
		return nil, errors.New("invalid salt size")
	}
	
	// 使用PBKDF2派生密钥
	key := pbkdf2.Key([]byte(password), salt, PBKDF2Iterations, KeySize, sha256.New)
	
	return &Crypto{key: key}, nil
}

// NewCryptoFromKey 从密钥直接创建加密器
func NewCryptoFromKey(key []byte) (*Crypto, error) {
	if len(key) != KeySize {
		return nil, errors.New("invalid key size, must be 32 bytes")
	}
	
	keyCopy := make([]byte, KeySize)
	copy(keyCopy, key)
	
	return &Crypto{key: keyCopy}, nil
}

// Encrypt 加密数据
func (c *Crypto) Encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	
	// 生成随机nonce
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	
	// 加密并附加nonce到开头
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt 解密数据
func (c *Crypto) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < NonceSize {
		return nil, errors.New("ciphertext too short")
	}
	
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	
	// 提取nonce
	nonce := ciphertext[:NonceSize]
	ciphertext = ciphertext[NonceSize:]
	
	// 解密
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	
	return plaintext, nil
}

// GenerateSalt 生成随机salt
func GenerateSalt() ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// ZeroBytes 安全清零字节数组
func ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Close 清理密钥
func (c *Crypto) Close() {
	ZeroBytes(c.key)
}

