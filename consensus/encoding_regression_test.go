package consensus

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉住 README“稳定编码规则”下的确切字节，而不仅是两次运行相等：
// 字段次序、固定前缀及其末尾换行、Base64 填充、uint64 十进制表示或
// 签名字段位置中的任何一项发生偏离，即使本地签名与验签同时采用变化后的
// 表示，这些预期常量也会失配并指出与公开约定不一致。

// stableSigningPrefix 是公开签名编码的固定前缀（含末尾换行）。
const stableSigningPrefix = "consensus-lab-tx-v1\n"

// stableSenderB64 是回归向量使用的发送者公钥：
// 由确定性种子 0x01,0x02,...,0x20 派生的 Ed25519 公钥，任何人都可重建。
const stableSenderB64 = "ebVWLo/mVPlAeLES6KmLp5AfhTrmlb7X4OORC60ElmQ="

func stablePrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

// encVector 的每个预期值都可依据 README 的公开规则独立核对：
// canonicalDoc 按字段名手工拼接，签名是标准 Ed25519 对“前缀+文档”的签名，
// 标识是含签名字段文档的 SHA-256 十六进制摘要。
type encVector struct {
	name         string
	sequence     uint64
	content      []byte
	contentB64   string
	fee          uint64
	expiry       uint64
	signatureB64 string
	id           string
}

func stableVectors() []encVector {
	return []encVector{
		{
			name:         "basic",
			sequence:     7,
			content:      []byte("hello"),
			contentB64:   "aGVsbG8=",
			fee:          3,
			expiry:       99,
			signatureB64: "VGlOKxKGlUwKRoBNoyt+ddE1gtTbBAMc3tnq8s0by5nmGrULjLZWprlvGitRyeIw/fI/GJfGzfBSgaosGLm1Aw==",
			id:           "87ba6a9dbb6b733b36ed8722ea272fa8dc447faf44e368ec6048ce5597099bb0",
		},
		{
			name:         "empty-content-zero-fee",
			sequence:     1,
			content:      nil,
			contentB64:   "",
			fee:          0,
			expiry:       2,
			signatureB64: "Ow/OKCXgtdELBbQJahkOy5fwywpCi1KxFZDD9j3ap2aDhun2MW4POYmiYG6j7pez45aMKQSunDPq+wMNuQgbDw==",
			id:           "efd76322ec3b683d1557a712896ca6f21dce9fb11a754b4eb5c96f2fecf9417f",
		},
		{
			// 内容含零字节且不是合法 UTF-8（0xff、0xfe），原始字节必须完整保留。
			name:         "binary-content-max-expiry",
			sequence:     5,
			content:      []byte{0x00, 0xff, 0xfe, 0x61},
			contentB64:   "AP/+YQ==",
			fee:          0,
			expiry:       18446744073709551615,
			signatureB64: "e/jxKGPp96dKtRgqQM1fXDbRL7ndc4qWo5UxvY53j4AzD/MACAWNqM3aBJjXE+90tLuZftrBtGDTDqV5BANPCQ==",
			id:           "d99c693f8c7282bb56ec9540785a91598d37084726312b975ea56869b0dcadf8",
		},
		{
			// 序号、费用、到期轮次同时取 uint64 最大值，必须保持完整十进制整数。
			name:         "uint64-max",
			sequence:     18446744073709551615,
			content:      []byte{0xff, 0xff},
			contentB64:   "//8=",
			fee:          18446744073709551615,
			expiry:       18446744073709551615,
			signatureB64: "yQlfTJNO2McZA6gaAXL9wLl5idTD+3/aX4mkGAdFD4AbazmD1ApC7U7aFidhX8vwIFv09V7cXva8kTg3b4yqBg==",
			id:           "edd03fca14f64e37fbd47adddc66c8d8b492d56e40d474baee4539180856925c",
		},
	}
}

// canonicalTxDoc 按 README 的公开规则手工拼接规范 JSON：
// 字段按名字典序、无任何多余空白；signatureB64 为空时省略签名字段。
func canonicalTxDoc(v encVector, signatureB64 string) string {
	doc := fmt.Sprintf(
		`{"content":%q,"expiry":%d,"fee":%d,"sender":%q,"sequence":%d`,
		v.contentB64, v.expiry, v.fee, stableSenderB64, v.sequence,
	)
	if signatureB64 != "" {
		doc += fmt.Sprintf(`,"signature":%q`, signatureB64)
	}
	return doc + "}"
}

func mustB64Decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("invalid base64 %q: %v", s, err)
	}
	return b
}

// jsonKeyOrder 返回 JSON 对象顶层键的实际出现次序。
func jsonKeyOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %s", raw)
	}
	var order []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, key.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return order
}

// 待签名字节必须是公开的固定前缀（含末尾换行）加无空白、字段按名字典序
// 排列的 JSON；签名本身不进入待签名字节，符合这些字节的标准 Ed25519
// 签名必须通过现有验签。
func TestSigningBytesExactVectors(t *testing.T) {
	priv := stablePrivateKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	for _, v := range stableVectors() {
		t.Run(v.name, func(t *testing.T) {
			tx := &Transaction{
				Sender:   append([]byte(nil), pub...),
				Sequence: v.sequence,
				Content:  append([]byte(nil), v.content...),
				Fee:      v.fee,
				Expiry:   v.expiry,
			}
			signing := tx.SigningBytes()
			wantDoc := canonicalTxDoc(v, "")
			want := []byte(stableSigningPrefix + wantDoc)

			// 钉住确切字节：次序、换行、填充或十进制表示一旦变化即失配。
			if !bytes.Equal(signing, want) {
				t.Fatalf("signing bytes mismatch:\n got %q\nwant %q", signing, want)
			}

			// 固定前缀及其唯一的末尾换行：前缀后必须紧接 JSON。
			if !bytes.HasPrefix(signing, []byte(stableSigningPrefix)) {
				t.Fatalf("missing fixed prefix %q", stableSigningPrefix)
			}
			jsonPart := signing[len(stableSigningPrefix):]
			if len(jsonPart) == 0 || jsonPart[0] != '{' || jsonPart[len(jsonPart)-1] != '}' {
				t.Fatalf("prefix must be followed immediately by the JSON object: %q", signing)
			}

			// JSON 内不得出现任何空白。
			if bytes.ContainsAny(jsonPart, " \t\r\n") {
				t.Fatalf("canonical JSON must contain no whitespace: %q", jsonPart)
			}

			// 字段次序：content < expiry < fee < sender < sequence（按名字典序）。
			if order := jsonKeyOrder(t, jsonPart); strings.Join(order, ",") !=
				"content,expiry,fee,sender,sequence" {
				t.Fatalf("unexpected field order: %v", order)
			}

			// 序号、费用、到期轮次是无符号整数的十进制表示：不带引号、
			// 不带小数点/指数，且与 strconv 十进制结果逐字一致。
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(jsonPart, &fields); err != nil {
				t.Fatal(err)
			}
			decimals := map[string]uint64{
				"sequence": v.sequence,
				"fee":      v.fee,
				"expiry":   v.expiry,
			}
			for name, num := range decimals {
				raw := string(fields[name])
				if raw != strconv.FormatUint(num, 10) {
					t.Fatalf("field %s encoded as %q, want plain decimal %d", name, raw, num)
				}
				if raw[0] == '"' || strings.ContainsAny(raw, ".eE+") {
					t.Fatalf("field %s must be a bare JSON integer, got %q", name, raw)
				}
			}

			// 发送者与内容为标准 Base64（带填充），且内容字节可完整还原。
			var view struct {
				Sender  string `json:"sender"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(jsonPart, &view); err != nil {
				t.Fatal(err)
			}
			if view.Sender != stableSenderB64 {
				t.Fatalf("sender base64 = %q, want %q", view.Sender, stableSenderB64)
			}
			if view.Content != v.contentB64 {
				t.Fatalf("content base64 = %q, want %q", view.Content, v.contentB64)
			}
			if got := mustB64Decode(t, view.Content); !bytes.Equal(got, v.content) {
				t.Fatalf("content bytes not preserved: got %v, want %v", got, v.content)
			}

			// 签名本身不进入待签名字节。
			if bytes.Contains(signing, []byte("signature")) {
				t.Fatalf("signing bytes must not contain the signature field: %q", signing)
			}

			// 钉住的标准 Ed25519 签名必须通过现有验签与独立验签。
			sig := mustB64Decode(t, v.signatureB64)
			tx.Signature = sig
			if !tx.Verify() {
				t.Fatal("pinned signature must verify via Transaction.Verify")
			}
			if !ed25519.Verify(pub, want, sig) {
				t.Fatal("pinned signature must verify against the pinned bytes with stdlib")
			}
			// 反向钉住向量本身：对公开字节重新签名必须得到同一签名。
			if got := ed25519.Sign(priv, want); !bytes.Equal(got, sig) {
				t.Fatalf("signature over public bytes differs from pinned vector")
			}

			// 前缀末尾换行或任何字节被改动后，同一签名必须失效——
			// 这证明钉住的是公开字节约定，而非本地自洽的另一套表示。
			if ed25519.Verify(pub, append(append([]byte{}, signing...), '\n'), sig) {
				t.Fatal("appending a byte must invalidate the pinned signature")
			}
			moved := append([]byte{}, signing...)
			moved[len(stableSigningPrefix)-1] = 'X' // 破坏前缀末尾换行
			if ed25519.Verify(pub, moved, sig) {
				t.Fatal("changing the prefix newline must invalidate the pinned signature")
			}
			if ed25519.Verify(pub, jsonPart, sig) {
				t.Fatal("dropping the fixed prefix must invalidate the pinned signature")
			}
		})
	}
}

// 交易标识必须是含签名字段的规范 JSON 的 SHA-256 十六进制摘要：
// 不带签名前缀，签名字段为标准 Base64 且按字段名排在 sequence 之后，
// 输出为小写 64 位；重新提供相同字段与签名字节必须得到相同标识。
func TestTransactionIDExactVectors(t *testing.T) {
	priv := stablePrivateKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	for _, v := range stableVectors() {
		t.Run(v.name, func(t *testing.T) {
			sig := mustB64Decode(t, v.signatureB64)
			tx := &Transaction{
				Sender:    append([]byte(nil), pub...),
				Sequence:  v.sequence,
				Content:   append([]byte(nil), v.content...),
				Fee:       v.fee,
				Expiry:    v.expiry,
				Signature: append([]byte(nil), sig...),
			}

			id := tx.ID()

			// 钉住摘要结果。
			if id != v.id {
				t.Fatalf("id = %s, want %s", id, v.id)
			}
			// 小写 64 位十六进制。
			if len(id) != 64 || id != strings.ToLower(id) {
				t.Fatalf("id must be 64 lowercase hex chars, got %q", id)
			}
			if _, err := hex.DecodeString(id); err != nil {
				t.Fatalf("id is not valid hex: %v", err)
			}

			// 依据公开规则独立重算：摘要对象是含签名的文档，不带签名前缀。
			wantIDDoc := canonicalTxDoc(v, v.signatureB64)
			sum := sha256.Sum256([]byte(wantIDDoc))
			if got := hex.EncodeToString(sum[:]); got != id {
				t.Fatalf("independent digest %s != %s", got, id)
			}

			// 签名字段按字段名排序，位于 sequence 之后；JSON 无空白。
			if order := jsonKeyOrder(t, []byte(wantIDDoc)); strings.Join(order, ",") !=
				"content,expiry,fee,sender,sequence,signature" {
				t.Fatalf("unexpected id doc field order: %v", order)
			}
			if strings.ContainsAny(wantIDDoc, " \t\r\n") {
				t.Fatalf("id doc must contain no whitespace: %s", wantIDDoc)
			}
			// 摘要内容明确不含签名前缀。
			prefixedSum := sha256.Sum256([]byte(stableSigningPrefix + wantIDDoc))
			if id == hex.EncodeToString(prefixedSum[:]) {
				t.Fatal("id must hash the JSON document without the signing prefix")
			}

			// 重新提供相同字段和签名字节，仍应得到相同标识。
			tx2 := &Transaction{
				Sender:    append([]byte(nil), pub...),
				Sequence:  v.sequence,
				Content:   append([]byte(nil), v.content...),
				Fee:       v.fee,
				Expiry:    v.expiry,
				Signature: append([]byte(nil), sig...),
			}
			if tx2.ID() != id {
				t.Fatal("re-supplied identical fields and signature must yield the same id")
			}

			// 用私钥重新签名应得到同一签名字节（同一公开待签名文本），
			// 因而标识也保持不变。
			tx3 := &Transaction{
				Sender:   append([]byte(nil), pub...),
				Sequence: v.sequence,
				Content:  append([]byte(nil), v.content...),
				Fee:      v.fee,
				Expiry:   v.expiry,
			}
			tx3.Sign(priv)
			if !bytes.Equal(tx3.Signature, sig) {
				t.Fatal("re-signing the same bytes must reproduce the pinned signature")
			}
			if tx3.ID() != id {
				t.Fatal("re-signed identical transaction must keep the same id")
			}
		})
	}
}

// 边界：内容未设置（nil）与零长度字节编码与标识相同；零值与 uint64
// 最大值保持完整十进制；二进制内容逐字节保留。
func TestEncodingBoundaries(t *testing.T) {
	priv := stablePrivateKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	t.Run("nil-and-empty-content-identical", func(t *testing.T) {
		mk := func(content []byte) *Transaction {
			return &Transaction{
				Sender:    append([]byte(nil), pub...),
				Sequence:  1,
				Content:   content,
				Fee:       0,
				Expiry:    2,
				Signature: mustB64Decode(t, stableVectors()[1].signatureB64),
			}
		}
		nilTx := mk(nil)
		emptyTx := mk([]byte{})

		if !bytes.Equal(nilTx.SigningBytes(), emptyTx.SigningBytes()) {
			t.Fatalf("nil and empty content must share signing bytes:\n nil=%q\nempty=%q",
				nilTx.SigningBytes(), emptyTx.SigningBytes())
		}
		if nilTx.ID() != emptyTx.ID() {
			t.Fatalf("nil and empty content must share id: %s vs %s", nilTx.ID(), emptyTx.ID())
		}
		// 与钉住的空内容向量逐字节一致。
		v := stableVectors()[1]
		want := []byte(stableSigningPrefix + canonicalTxDoc(v, ""))
		if !bytes.Equal(nilTx.SigningBytes(), want) {
			t.Fatalf("empty content encoding drift:\n got %q\nwant %q", nilTx.SigningBytes(), want)
		}
		if nilTx.ID() != v.id {
			t.Fatalf("empty content id = %s, want %s", nilTx.ID(), v.id)
		}
	})

	t.Run("zero-fee-is-plain-zero", func(t *testing.T) {
		tx := &Transaction{Sender: append([]byte(nil), pub...), Sequence: 1, Fee: 0, Expiry: 2}
		sb := string(tx.SigningBytes())
		if !strings.Contains(sb, `"fee":0`) {
			t.Fatalf("zero fee must be encoded as 0: %q", sb)
		}
		if strings.Contains(sb, `"fee":"0"`) || strings.Contains(sb, `"fee":0.0`) {
			t.Fatalf("fee must be a bare integer: %q", sb)
		}
	})

	t.Run("uint64-max-not-truncated", func(t *testing.T) {
		const maxUint = ^uint64(0)
		tx := &Transaction{
			Sender:   append([]byte(nil), pub...),
			Sequence: maxUint,
			Content:  []byte{0xff, 0xff},
			Fee:      maxUint,
			Expiry:   maxUint,
		}
		sb := tx.SigningBytes()
		full := "18446744073709551615"
		for _, field := range []string{`"sequence":`, `"fee":`, `"expiry":`} {
			if !bytes.Contains(sb, []byte(field+full)) {
				t.Fatalf("uint64 max for %s must be the full decimal %s: %q", field, full, sb)
			}
		}
		// 数值字段不得带引号、小数点或指数：解码后逐个检查原始 token。
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(sb[len(stableSigningPrefix):], &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"sequence", "fee", "expiry"} {
			raw := string(fields[name])
			if raw != full {
				t.Fatalf("field %s encoded as %q, want bare %s", name, raw, full)
			}
		}
		// 与钉住向量逐字节一致。
		v := stableVectors()[3]
		if want := []byte(stableSigningPrefix + canonicalTxDoc(v, "")); !bytes.Equal(sb, want) {
			t.Fatalf("uint64 max encoding drift:\n got %q\nwant %q", sb, want)
		}
	})

	t.Run("binary-content-preserved", func(t *testing.T) {
		raw := []byte{0x00, 0xff, 0xfe, 0x61}
		tx := &Transaction{
			Sender:   append([]byte(nil), pub...),
			Sequence: 5,
			Content:  raw,
			Fee:      0,
			Expiry:   ^uint64(0),
		}
		sb := string(tx.SigningBytes())
		if !strings.Contains(sb, `"content":"AP/+YQ=="`) {
			t.Fatalf("binary content must be standard padded base64 AP/+YQ==: %q", sb)
		}
		// 编码不得尝试把字节当 UTF-8 文本处理。
		if strings.Contains(sb, string(raw)) {
			t.Fatalf("raw non-UTF-8 bytes must not appear unencoded: %q", sb)
		}
		tx.Sign(priv)
		if !tx.Verify() {
			t.Fatal("transaction with binary content must verify")
		}
		// 与钉住的二进制内容向量逐字节一致（含标识）。
		v := stableVectors()[2]
		if want := stableSigningPrefix + canonicalTxDoc(v, ""); string(tx.SigningBytes()) != want {
			t.Fatalf("binary content encoding drift:\n got %q\nwant %q", tx.SigningBytes(), want)
		}
		if !bytes.Equal(tx.Signature, mustB64Decode(t, v.signatureB64)) {
			t.Fatal("binary content signature drifted from pinned vector")
		}
		if tx.ID() != v.id {
			t.Fatalf("binary content id = %s, want %s", tx.ID(), v.id)
		}
	})
}

// 公开接口层面：按公开规则准备并签名的交易提交后，返回的标识必须等于
// 钉住的摘要；接收行为保持不变。
func TestStableEncodingThroughSubmit(t *testing.T) {
	priv := stablePrivateKey(t)
	pub := priv.Public().(ed25519.PublicKey)

	n, err := New(t.TempDir(), Config{
		Seed:           []byte("stable-encoding-regression"),
		Validators:     [][]byte{pub},
		MaxTxsPerBlock: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	v := stableVectors()[0] // sequence 7, fee 3, expiry 99（允许序号缺口入池）
	tx := &Transaction{
		Sender:    append([]byte(nil), pub...),
		Sequence:  v.sequence,
		Content:   append([]byte(nil), v.content...),
		Fee:       v.fee,
		Expiry:    v.expiry,
		Signature: mustB64Decode(t, v.signatureB64),
	}
	if !tx.Verify() {
		t.Fatal("pinned transaction must verify before submit")
	}
	res, err := n.Submit(tx)
	if err != nil {
		t.Fatalf("pinned transaction must be accepted: %v", err)
	}
	if res.TxID != v.id {
		t.Fatalf("submit returned id %s, want pinned %s", res.TxID, v.id)
	}
	info, err := n.Tx(v.id)
	if err != nil || info == nil || info.Status != StatusQueued || info.ID != v.id {
		t.Fatalf("accepted tx must be queryable by the pinned id: %+v (%v)", info, err)
	}
}
