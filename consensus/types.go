package consensus

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// 交易被拒绝的具体原因。
const (
	// ReasonBadSignature 签名非法或与发送者公钥不匹配。
	ReasonBadSignature = "bad-signature"
	// ReasonExpired 交易在提交时已经到期（到期轮次不大于当前轮次）。
	ReasonExpired = "tx-expired"
	// ReasonOldSequence 交易序号不大于该发送者已确认序号。
	ReasonOldSequence = "sequence-already-confirmed"
	// ReasonDuplicate 池中已有完全相同的交易（同一发送者、同一序号、同一标识）。
	ReasonDuplicate = "duplicate-transaction"
	// ReasonLowFee 池中同发送者同序号已有交易，但新交易费用没有严格更高。
	ReasonLowFee = "fee-not-higher"
	// ReasonNotValidator 投票者不在验证者名单内。
	ReasonNotValidator = "not-validator"
	// ReasonWrongRound 投票轮次与当前轮次不一致。
	ReasonWrongRound = "wrong-round"
	// ReasonWrongBlock 投票区块标识与当前提议区块标识不一致。
	ReasonWrongBlock = "wrong-block-id"
	// ReasonNoProposal 当前轮次尚不存在提议。
	ReasonNoProposal = "no-proposal"
	// ReasonAlreadyVoted 该验证者本轮已投过其他候选，不能改投。
	ReasonAlreadyVoted = "already-voted"
	// ReasonUnknownRound 查询的轮次不存在（为 0 或尚未到达）。
	ReasonUnknownRound = "unknown-round"
)

// TxStatus 表示一笔交易相对于本节点当前状态的状态。
type TxStatus string

const (
	// StatusQueued 在交易池中等待打包。
	StatusQueued TxStatus = "queued"
	// StatusProposed 已进入当前轮次提议，等待投票。
	StatusProposed TxStatus = "proposed"
	// StatusConfirmed 已被某个确认块包含。
	StatusConfirmed TxStatus = "confirmed"
	// StatusReplaced 在交易池中被同发送者同序号、费用严格更高的交易替换。
	StatusReplaced TxStatus = "replaced"
	// StatusExpired 在进入新轮次时因到期轮次不大于当前轮次而失效。
	StatusExpired TxStatus = "expired"
)

// Transaction 是一笔带 Ed25519 签名的交易。
type Transaction struct {
	// Sender 发送者公钥（Ed25519 原始 32 字节公钥）。
	Sender []byte `json:"sender"`
	// Sequence 正整数序号。
	Sequence uint64 `json:"sequence"`
	// Content 交易内容，可为任意字节。
	Content []byte `json:"content"`
	// Fee 非负整数费用。
	Fee uint64 `json:"fee"`
	// Expiry 到期轮次：进入不小于该值的轮次时交易失效。
	Expiry uint64 `json:"expiry"`
	// Signature 对 SigningBytes 的 Ed25519 签名。
	Signature []byte `json:"signature"`
}

// signDoc 是签名与稳定编码使用的规范结构。字段按名字典序声明，
// Go 的 JSON 编码按结构体字段顺序输出，故序列化结果即公开的签名编码。
type signDoc struct {
	Content  string `json:"content"`
	Expiry   uint64 `json:"expiry"`
	Fee      uint64 `json:"fee"`
	Sender   string `json:"sender"`
	Sequence uint64 `json:"sequence"`
}

func docFromTx(tx *Transaction) signDoc {
	return signDoc{
		Sender:   base64.StdEncoding.EncodeToString(tx.Sender),
		Sequence: tx.Sequence,
		Content:  base64.StdEncoding.EncodeToString(tx.Content),
		Fee:      tx.Fee,
		Expiry:   tx.Expiry,
	}
}

// SigningBytes 返回交易的公开稳定签名编码：
// canonical JSON（UTF-8、无空白、字段按名字典序）外再包一层固定前缀。
// 任何语言按相同 JSON 规则即可重建并验签。
func SigningBytes(sender ed25519.PublicKey, sequence uint64, content []byte, fee, expiry uint64) []byte {
	tx := &Transaction{Sender: sender, Sequence: sequence, Content: content, Fee: fee, Expiry: expiry}
	raw, _ := json.Marshal(docFromTx(tx))
	out := make([]byte, 0, len("consensus-lab-tx-v1\n")+len(raw))
	out = append(out, []byte("consensus-lab-tx-v1\n")...)
	out = append(out, raw...)
	return out
}

// SigningBytes 返回该交易待签名的字节。
func (tx *Transaction) SigningBytes() []byte {
	return SigningBytes(tx.Sender, tx.Sequence, tx.Content, tx.Fee, tx.Expiry)
}

// Sign 使用发送者私钥填写 Signature。
func (tx *Transaction) Sign(priv ed25519.PrivateKey) {
	tx.Signature = append([]byte(nil), ed25519.Sign(priv, tx.SigningBytes())...)
}

// NewTransaction 构造并用 priv 签名一笔交易。
func NewTransaction(priv ed25519.PrivateKey, sequence uint64, content []byte, fee, expiry uint64) *Transaction {
	pub := priv.Public().(ed25519.PublicKey)
	tx := &Transaction{
		Sender:   append([]byte(nil), pub...),
		Sequence: sequence,
		Content:  append([]byte(nil), content...),
		Fee:      fee,
		Expiry:   expiry,
	}
	tx.Sign(priv)
	return tx
}

// Verify 校验公钥长度、签名长度以及签名是否有效。
func (tx *Transaction) Verify() bool {
	if len(tx.Sender) != ed25519.PublicKeySize || len(tx.Signature) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(tx.Sender), tx.SigningBytes(), tx.Signature)
}

// ID 返回交易标识：规范 JSON 字段（含签名）的 SHA-256 十六进制摘要。
// 该规则公开稳定，同字节内容恒有同标识。
func (tx *Transaction) ID() string {
	type txDoc struct {
		Content   string `json:"content"`
		Expiry    uint64 `json:"expiry"`
		Fee       uint64 `json:"fee"`
		Sender    string `json:"sender"`
		Sequence  uint64 `json:"sequence"`
		Signature string `json:"signature"`
	}
	raw, _ := json.Marshal(txDoc{
		Sender:    base64.StdEncoding.EncodeToString(tx.Sender),
		Sequence:  tx.Sequence,
		Content:   base64.StdEncoding.EncodeToString(tx.Content),
		Fee:       tx.Fee,
		Expiry:    tx.Expiry,
		Signature: base64.StdEncoding.EncodeToString(tx.Signature),
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SenderHex 返回发送者公钥的十六进制串，便于按账户查询。
func (tx *Transaction) SenderHex() string {
	return hex.EncodeToString(tx.Sender)
}

// Vote 是验证者对某一轮某区块标识的投票。
type Vote struct {
	Validator []byte `json:"validator"`
	Round     uint64 `json:"round"`
	BlockID   string `json:"block_id"`
}

// BlockID 是由轮次、前块标识与交易顺序确定的区块标识。
// 对规范文档（轮次、高度、前块标识、按顺序排列的交易标识）取 SHA-256。
func BlockID(round, height uint64, prevID string, txIDs []string) string {
	type blockDoc struct {
		Round        uint64   `json:"round"`
		Height       uint64   `json:"height"`
		PreviousID   string   `json:"previous_id"`
		Transactions []string `json:"transactions"`
	}
	ids := txIDs
	if ids == nil {
		ids = []string{}
	}
	raw, _ := json.Marshal(blockDoc{Round: round, Height: height, PreviousID: prevID, Transactions: ids})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Block 是一条连续的确认历史记录。
type Block struct {
	Height     uint64   `json:"height"`
	Round      uint64   `json:"round"`
	ID         string   `json:"id"`
	PreviousID string   `json:"previous_id"`
	TxIDs      []string `json:"tx_ids"`
}

// TxInfo 是一笔交易的查询结果。
type TxInfo struct {
	// ID 交易标识。
	ID string
	// Status 当前状态。
	Status TxStatus
	// Tx 交易内容（历史记录中同样保留完整字节）。
	Tx *Transaction
	// BlockHeight 已确认时所在区块高度；其他状态为 0。
	BlockHeight uint64
	// BlockID 已确认时所在区块标识。
	BlockID string
	// ReplacedBy 已替换时的新交易标识。
	ReplacedBy string
	// Note 仅用于待处理交易："waiting-vote"（已提议，等待投票）或 "waiting-pack"（池内，等待打包）。
	Note string
}

// AccountInfo 是按账户查询的结果。
type AccountInfo struct {
	Sender string
	// ConfirmedSequence 已确认序号，初值为 0。
	ConfirmedSequence uint64
	// Pending 该账户全部待处理交易（池内排队 + 已提议），按序号升序。
	Pending []*TxInfo
	// Gap 因序号缺口等待时，最早缺少的序号；无缺口为 0。
	Gap uint64
}

// RejectError 描述一次提交或投票被拒绝的具体原因。
type RejectError struct {
	Reason string
	Detail string
}

func (e *RejectError) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return fmt.Sprintf("%s: %s", e.Reason, e.Detail)
}

func reject(reason, format string, args ...any) error {
	return &RejectError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}
