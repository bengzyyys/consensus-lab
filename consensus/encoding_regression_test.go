package consensus

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

// 本文件把交易的公开稳定编码（README「稳定编码规则」）钉死在确切的
// 黄金字节上：签名编码、签名本身与交易标识都与按公开规则独立算出的
// 字面值比较。这样即使签名与验签同时改用另一种内部表示（两次运行
// 依然相等、互验依然通过），只要偏离公开约定就会被发现。

// 固定测试私钥种子，使 Ed25519 签名确定可复现。
var goldenSeed = []byte("0123456789abcdef0123456789abcdef")

func goldenKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	if len(goldenSeed) != ed25519.SeedSize {
		t.Fatal("golden seed must be 32 bytes")
	}
	return ed25519.NewKeyFromSeed(goldenSeed)
}

// 黄金值：sender 公钥的标准 Base64（带填充）。
const goldenSenderB64 = "I7xUkSwebpLEqGglyGfif/3FVb/71CRPF6Jqv//ull0="

// 签名编码必须是公开的确切字节：固定前缀（含末尾换行）+ 无空白、
// 字段按名字典序的 canonical JSON，且签名本身不进入待签名字节。
func TestSigningBytesGolden(t *testing.T) {
	priv := goldenKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	tx := NewTransaction(priv, 7, []byte("hello"), 3, 99)

	want := "consensus-lab-tx-v1\n" +
		`{"content":"aGVsbG8=","expiry":99,"fee":3,"sender":"` + goldenSenderB64 + `","sequence":7}`

	if got := tx.SigningBytes(); string(got) != want {
		t.Fatalf("SigningBytes mismatch:\n got %q\nwant %q", got, want)
	}
	// 包级函数与方法的编码必须一致。
	if got := SigningBytes(pub, 7, []byte("hello"), 3, 99); string(got) != want {
		t.Fatalf("package-level SigningBytes mismatch:\n got %q\nwant %q", got, want)
	}
	// 前缀之后不允许出现任何额外空白（无缩进、无空格、无第二个换行）。
	if rest := string(tx.SigningBytes()[len("consensus-lab-tx-v1\n"):]); strings.ContainsAny(rest, " \t\n\r") {
		t.Fatalf("canonical JSON must contain no whitespace, got %q", rest)
	}

	// 签名是对上述确切字节的标准 Ed25519 签名，且是确定值。
	wantSig, err := base64.StdEncoding.DecodeString("b9wyilMPNGZUdBxsOR+2sJtds61h/xDLX1i9+uiWZMxNU/NL0cMHuZ5YCGEh2Df4FR0FC3Xha7rRfgBNOy0xCQ==")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tx.Signature, wantSig) {
		t.Fatalf("signature = %x, want %x", tx.Signature, wantSig)
	}
	// 用标准库直接对黄金字节验签，不经过本包的任何编码函数。
	if !ed25519.Verify(pub, []byte(want), wantSig) {
		t.Fatal("golden signature must verify against golden bytes with plain ed25519")
	}
	// 现有验签功能必须接受该签名。
	if !tx.Verify() {
		t.Fatal("Verify must accept a standard Ed25519 signature over the public bytes")
	}
}

// 交易标识是含签名字段的 canonical JSON（不带签名前缀）的
// SHA-256 小写十六进制摘要，签名字段按名字典序位于 sequence 之后。
func TestTxIDGolden(t *testing.T) {
	priv := goldenKey(t)
	tx := NewTransaction(priv, 7, []byte("hello"), 3, 99)

	const wantID = "e9996304731b59dba0d021850795f853442964d668009cf44e27ff7557f09ce2"
	if tx.ID() != wantID {
		t.Fatalf("ID = %s, want %s", tx.ID(), wantID)
	}
	// 输出保持小写、六十四位十六进制。
	if len(tx.ID()) != 64 {
		t.Fatalf("ID length = %d, want 64", len(tx.ID()))
	}
	for _, c := range tx.ID() {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("ID must be lowercase hex, got %q", tx.ID())
		}
	}

	// 重新提供相同字段和相同签名字节，必须得到相同标识。
	clone := &Transaction{
		Sender:    append([]byte(nil), tx.Sender...),
		Sequence:  tx.Sequence,
		Content:   append([]byte(nil), tx.Content...),
		Fee:       tx.Fee,
		Expiry:    tx.Expiry,
		Signature: append([]byte(nil), tx.Signature...),
	}
	if clone.ID() != wantID {
		t.Fatalf("re-supplied fields+signature: ID = %s, want %s", clone.ID(), wantID)
	}
}

// 序号、费用、到期轮次取 uint64 最大值时，编码保持完整的十进制
// 无符号整数：不变成浮点、不带引号、不截断。
func TestSigningBytesGoldenUint64Max(t *testing.T) {
	priv := goldenKey(t)
	m := uint64(math.MaxUint64)

	tx := NewTransaction(priv, m, nil, m, m)

	want := "consensus-lab-tx-v1\n" +
		`{"content":"","expiry":18446744073709551615,"fee":18446744073709551615,"sender":"` + goldenSenderB64 + `","sequence":18446744073709551615}`
	if got := tx.SigningBytes(); string(got) != want {
		t.Fatalf("SigningBytes mismatch:\n got %q\nwant %q", got, want)
	}
	if !tx.Verify() {
		t.Fatal("max-uint64 transaction must verify")
	}

	const wantID = "c787af189db46050966e4815e95a5473c2b742bfaa813b1bd2ffdbeb445912e2"
	if tx.ID() != wantID {
		t.Fatalf("ID = %s, want %s", tx.ID(), wantID)
	}
}

// 内容为空时，未设置内容（nil）与零长度字节必须具有相同编码和标识，
// 编码为 "content":""（标准 Base64 的空串）。
func TestEmptyContentEncoding(t *testing.T) {
	priv := goldenKey(t)

	txNil := NewTransaction(priv, 1, nil, 5, 10)
	txEmpty := NewTransaction(priv, 1, []byte{}, 5, 10)

	if !bytes.Equal(txNil.SigningBytes(), txEmpty.SigningBytes()) {
		t.Fatalf("nil and empty content differ:\n nil   %q\n empty %q", txNil.SigningBytes(), txEmpty.SigningBytes())
	}
	if txNil.ID() != txEmpty.ID() {
		t.Fatalf("nil and empty content ids differ: %s vs %s", txNil.ID(), txEmpty.ID())
	}
	if !strings.Contains(string(txNil.SigningBytes()), `"content":""`) {
		t.Fatalf("empty content must encode as \"content\":\"\", got %q", txNil.SigningBytes())
	}
	if !txNil.Verify() || !txEmpty.Verify() {
		t.Fatal("empty-content transactions must verify")
	}
}

// 内容包含零字节及非 UTF-8 字节时，原始字节必须完整保留：
// 编码中的 Base64 解码回去逐字节等于原内容。
func TestBinaryContentPreserved(t *testing.T) {
	priv := goldenKey(t)
	content := []byte{0x00, 0xff, 0x80, 0x41, 0x00, 0xfe}

	tx := NewTransaction(priv, 2, content, 5, 10)

	// 标准 Base64（带填充）的黄金表示，可对照公开规则手工核对。
	const wantB64 = "AP+AQQD+"
	if !strings.Contains(string(tx.SigningBytes()), `"content":"`+wantB64+`"`) {
		t.Fatalf("binary content must encode as base64 %q, got %q", wantB64, tx.SigningBytes())
	}
	decoded, err := base64.StdEncoding.DecodeString(wantB64)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, content) {
		t.Fatalf("decoded content = %x, want original %x", decoded, content)
	}
	// 交易本身保留完整原始字节，且签名有效。
	if !bytes.Equal(tx.Content, content) {
		t.Fatalf("tx.Content = %x, want %x", tx.Content, content)
	}
	if !tx.Verify() {
		t.Fatal("binary-content transaction must verify")
	}
	// 重新构造相同字节，标识稳定。
	tx2 := NewTransaction(priv, 2, append([]byte(nil), content...), 5, 10)
	if tx2.ID() != tx.ID() {
		t.Fatalf("same binary content must yield same id: %s vs %s", tx2.ID(), tx.ID())
	}
}

// 费用为零时编码为不带引号的十进制 0。
func TestZeroFeeEncoding(t *testing.T) {
	priv := goldenKey(t)
	tx := NewTransaction(priv, 3, []byte("x"), 0, 10)

	if !strings.Contains(string(tx.SigningBytes()), `"fee":0,`) {
		t.Fatalf("zero fee must encode as unquoted 0, got %q", tx.SigningBytes())
	}
	if strings.Contains(string(tx.SigningBytes()), `"fee":"0"`) {
		t.Fatalf("zero fee must not be quoted, got %q", tx.SigningBytes())
	}
	if !tx.Verify() {
		t.Fatal("zero-fee transaction must verify")
	}
}

// 交易标识的摘要输入不含签名前缀：对同一笔交易，公开规则下的
// 摘要内容（含签名的 canonical JSON）可由公开字段重建并核对。
func TestTxIDDigestInputHasNoPrefix(t *testing.T) {
	priv := goldenKey(t)
	tx := NewTransaction(priv, 7, []byte("hello"), 3, 99)

	// 按公开规则手工重建摘要输入：签名编码去掉前缀、插入签名字段。
	sigB64 := base64.StdEncoding.EncodeToString(tx.Signature)
	wantJSON := `{"content":"aGVsbG8=","expiry":99,"fee":3,"sender":"` + goldenSenderB64 + `","sequence":7,"signature":"` + sigB64 + `"}`
	sum := sha256.Sum256([]byte(wantJSON))
	if got := tx.ID(); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("ID = %s, want sha256 of %q", got, wantJSON)
	}
	// 若摘要输入带前缀，结果必然不同（前缀不属于标识规则）。
	prefixed := sha256.Sum256([]byte("consensus-lab-tx-v1\n" + wantJSON))
	if tx.ID() == hex.EncodeToString(prefixed[:]) {
		t.Fatal("ID must not be computed over the signing prefix")
	}
}
