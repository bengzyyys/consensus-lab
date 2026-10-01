// Package consensus 是本地共识与交易池仿真的本地基线。
package consensus

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

const (
	// TxIDSize 是交易标识的字节长度（SHA-256）。
	TxIDSize = 32
	// HashSize 是区块标识的字节长度（SHA-256）。
	HashSize = 32
)

// TxID 是交易的稳定标识。
type TxID [TxIDSize]byte

// Hash 是区块的稳定标识。
type Hash [HashSize]byte

// Tx 是一笔交易。签名覆盖 Sender、Seq、Content、Fee、Expiry 的规范编码，
// 标识在签名之外还覆盖签名本身，因此不同签名的同一内容也是不同交易。
type Tx struct {
	Sender    []byte // 发送者 Ed25519 公钥（32 字节）
	Seq       uint64 // 正整数序号
	Content   []byte // 内容
	Fee       uint64 // 非负整数费用
	Expiry    uint64 // 到期轮次
	Signature []byte // Ed25519 签名
}

// Block 是一个确认块。Height 从 1 开始连续，PrevHash 链接前一块标识。
type Block struct {
	Height   uint64
	Round    uint64
	PrevHash Hash
	Proposer []byte // 出块验证者公钥
	TxIDs    []TxID // 交易顺序
}

func putUint64(b []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(b, buf[:]...)
}

func putUint32(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

// SigningPayload 返回交易的规范签名载荷：
// 固定 32 字节 Sender，随后依次为大端 8 字节 Seq、4 字节 Content 长度、
// Content 原文、8 字节 Fee、8 字节 Expiry。
func SigningPayload(tx *Tx) []byte {
	b := make([]byte, 0, 32+8+4+len(tx.Content)+8+8)
	b = append(b, tx.Sender...)
	b = putUint64(b, tx.Seq)
	b = putUint32(b, uint32(len(tx.Content)))
	b = append(b, tx.Content...)
	b = putUint64(b, tx.Fee)
	b = putUint64(b, tx.Expiry)
	return b
}

// TxIDOf 返回交易的稳定标识。标识在签名载荷基础上额外覆盖签名长度与签名。
func TxIDOf(tx *Tx) TxID {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(tx.Sender)))
	h.Write(buf[:])
	h.Write(tx.Sender)
	binary.BigEndian.PutUint64(buf[:], tx.Seq)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(len(tx.Content)))
	h.Write(buf[:])
	h.Write(tx.Content)
	binary.BigEndian.PutUint64(buf[:], tx.Fee)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], tx.Expiry)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], uint64(len(tx.Signature)))
	h.Write(buf[:])
	h.Write(tx.Signature)
	var id TxID
	copy(id[:], h.Sum(nil))
	return id
}

// ID 返回区块的稳定标识：依次覆盖大端 8 字节 Height、8 字节 Round、
// 32 字节 PrevHash、8 字节 Proposer 长度、Proposer、8 字节交易数及各交易标识。
func (blk *Block) ID() Hash {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], blk.Height)
	h.Write(buf[:])
	binary.BigEndian.PutUint64(buf[:], blk.Round)
	h.Write(buf[:])
	h.Write(blk.PrevHash[:])
	binary.BigEndian.PutUint64(buf[:], uint64(len(blk.Proposer)))
	h.Write(buf[:])
	h.Write(blk.Proposer)
	binary.BigEndian.PutUint64(buf[:], uint64(len(blk.TxIDs)))
	h.Write(buf[:])
	for _, id := range blk.TxIDs {
		h.Write(id[:])
	}
	var hash Hash
	copy(hash[:], h.Sum(nil))
	return hash
}

// SignTx 使用私钥对交易签名，填充 Signature 字段。
func SignTx(priv ed25519.PrivateKey, tx *Tx) {
	tx.Signature = ed25519.Sign(priv, SigningPayload(tx))
}

// GenerateKey 生成一对 Ed25519 密钥。
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodeHex 返回字节的小写十六进制编码。
func EncodeHex(b []byte) string { return hex.EncodeToString(b) }

// DecodeHex 解析十六进制字节，失败返回 nil。
func DecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
