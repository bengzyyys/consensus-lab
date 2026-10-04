package consensus

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉住 README“稳定编码规则”中区块标识的确切输出，而不仅是两次调用相等：
// 文档按 round、height、previous_id、transactions 的次序写成无额外空白的 JSON，
// 轮次与高度是完整的无符号十进制整数，前块标识与交易标识是字符串，
// 文档的 UTF-8 字节经 SHA-256 得到六十四位小写十六进制标识。
// 每个预期值都可依据上述公开规则独立核对（本文件中的预期摘要由手工拼接的
// 文档经 sha256sum 得出，与库当前输出一致）；字段次序、整数表示、交易列表
// 次序或摘要形式中的任何一项偏离，即使各入口一起换了编码方式也会在此失配。

// 回归向量使用的两个交易标识：取自签名编码回归向量（见
// encoding_regression_test.go），任何人都可依据公开规则重建。
const (
	blockVecTxA = "87ba6a9dbb6b733b36ed8722ea272fa8dc447faf44e368ec6048ce5597099bb0"
	blockVecTxB = "efd76322ec3b683d1557a712896ca6f21dce9fb11a754b4eb5c96f2fecf9417f"
)

// blockVector 的 doc 是按公开规则手工拼接的规范 JSON，id 是其 UTF-8 字节的
// SHA-256 小写十六进制摘要；两者都可脱离本库独立核对。
type blockVector struct {
	name   string
	round  uint64
	height uint64
	prevID string
	txIDs  []string
	doc    string
	id     string
}

func blockVectors() []blockVector {
	return []blockVector{
		{
			name:   "genesis-empty",
			round:  1,
			height: 1,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":1,"height":1,"previous_id":"","transactions":[]}`,
			id:     "ee9d28f151582c08818731640c00cf0dcbe3f9cff6e1ed4d7256ea7565791b7b",
		},
		{
			name:   "two-txs-order-a",
			round:  1,
			height: 1,
			prevID: "",
			txIDs:  []string{blockVecTxA, blockVecTxB},
			doc: `{"round":1,"height":1,"previous_id":"","transactions":["` +
				blockVecTxA + `","` + blockVecTxB + `"]}`,
			id: "49ce76b1e1f4b57ec49264a6f62c5e73a192e99a35d2795077ccb9beb4b14e13",
		},
		{
			// 同一对交易、相反顺序：交易列表保留给定顺序，生成标识时不排序。
			name:   "two-txs-order-b",
			round:  1,
			height: 1,
			prevID: "",
			txIDs:  []string{blockVecTxB, blockVecTxA},
			doc: `{"round":1,"height":1,"previous_id":"","transactions":["` +
				blockVecTxB + `","` + blockVecTxA + `"]}`,
			id: "44361f03d26ab8ed8daead90bd1f3c3b6df1b4b80f138987f44e024275be8057",
		},
		{
			// 轮次与高度取不同值：两者互换必须得到不同标识。
			name:   "round7-height3",
			round:  7,
			height: 3,
			prevID: "",
			txIDs:  []string{blockVecTxA},
			doc:    `{"round":7,"height":3,"previous_id":"","transactions":["` + blockVecTxA + `"]}`,
			id:     "3077077dae794cb40398c5ef2b0e041bac26993a2c069c7bece022e88a38de48",
		},
		{
			name:   "round3-height7",
			round:  3,
			height: 7,
			prevID: "",
			txIDs:  []string{blockVecTxA},
			doc:    `{"round":3,"height":7,"previous_id":"","transactions":["` + blockVecTxA + `"]}`,
			id:     "a924c88498503302dc0536798f97a105c8ee9e4338b95f7a1d3dd21b27661568",
		},
		{
			// 前块标识非空：接在 genesis-empty 块之后的第二高度块。
			name:   "chained-prev-id",
			round:  2,
			height: 2,
			prevID: "ee9d28f151582c08818731640c00cf0dcbe3f9cff6e1ed4d7256ea7565791b7b",
			txIDs:  []string{blockVecTxB},
			doc: `{"round":2,"height":2,"previous_id":"ee9d28f151582c08818731640c00cf0dcbe3f9cff6e1ed4d7256ea7565791b7b","transactions":["` +
				blockVecTxB + `"]}`,
			id: "058da4ef9bbbcd3e888998bf2c7f5a11e5d8b656a259b68140a7848d5938e0f4",
		},
		{
			// 轮次取 uint64 最大值：必须保持完整二十位十进制，不得截断或科学计数。
			name:   "uint64-max-round",
			round:  18446744073709551615,
			height: 1,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":18446744073709551615,"height":1,"previous_id":"","transactions":[]}`,
			id:     "6054123a975f7258b2b8be7e625a58ad27ecc2ad435f11b8a69b4fc7d8c429ef",
		},
		{
			// 高度取 uint64 最大值：与上一向量对称，防止把轮次当高度或丢失高位。
			name:   "uint64-max-height",
			round:  1,
			height: 18446744073709551615,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":1,"height":18446744073709551615,"previous_id":"","transactions":[]}`,
			id:     "1ba9fcf0ec830fb9d6bc8c51b3cb5ea0b1491350d71a14c4c7590626a27d4447",
		},
	}
}

// 区块标识必须等于钉住的摘要，且能按公开规则从手工拼接的文档独立复算：
// 字段次序、无空白、整数十进制表示、交易列表次序与摘要形式都被逐钉住。
func TestBlockIDExactVectors(t *testing.T) {
	for _, v := range blockVectors() {
		t.Run(v.name, func(t *testing.T) {
			id := BlockID(v.round, v.height, v.prevID, v.txIDs)

			// 钉住摘要结果。
			if id != v.id {
				t.Fatalf("block id = %s, want %s", id, v.id)
			}
			// 小写 64 位十六进制。
			if len(id) != 64 || id != strings.ToLower(id) {
				t.Fatalf("block id must be 64 lowercase hex chars, got %q", id)
			}
			if _, err := hex.DecodeString(id); err != nil {
				t.Fatalf("block id is not valid hex: %v", err)
			}

			// 依据公开规则独立重算：对手工拼接的文档取 SHA-256。
			sum := sha256.Sum256([]byte(v.doc))
			if got := hex.EncodeToString(sum[:]); got != id {
				t.Fatalf("independent digest of documented form %s != %s", got, id)
			}

			// 文档无空白，字段次序为 round、height、previous_id、transactions。
			if strings.ContainsAny(v.doc, " \t\r\n") {
				t.Fatalf("canonical doc must contain no whitespace: %s", v.doc)
			}
			if order := jsonKeyOrder(t, []byte(v.doc)); strings.Join(order, ",") !=
				"round,height,previous_id,transactions" {
				t.Fatalf("unexpected field order: %v", order)
			}

			// 轮次与高度是无符号整数的十进制表示：不带引号、小数点或指数。
			for name, num := range map[string]uint64{"round": v.round, "height": v.height} {
				needle := `"` + name + `":` + strconv.FormatUint(num, 10)
				if !strings.Contains(v.doc, needle) {
					t.Fatalf("doc must contain bare decimal %s: %s", needle, v.doc)
				}
				if strings.Contains(v.doc, `"`+name+`":"`) {
					t.Fatalf("field %s must not be quoted: %s", name, v.doc)
				}
			}

			// 前块标识与交易标识是 JSON 字符串。
			if !strings.Contains(v.doc, `"previous_id":"`+v.prevID+`"`) {
				t.Fatalf("previous_id must be a JSON string: %s", v.doc)
			}
			for _, txID := range v.txIDs {
				if !strings.Contains(v.doc, `"`+txID+`"`) {
					t.Fatalf("tx id must appear as a JSON string: %s", v.doc)
				}
			}

			// 交易列表按给定顺序逐字出现在文档中。
			var list strings.Builder
			list.WriteString(`"transactions":[`)
			for i, txID := range v.txIDs {
				if i > 0 {
					list.WriteString(",")
				}
				list.WriteString(`"` + txID + `"`)
			}
			list.WriteString("]")
			if !strings.Contains(v.doc, list.String()) {
				t.Fatalf("transactions must appear in the given order: %s", v.doc)
			}
		})
	}
}

// 空交易列表有确定标识：未提供列表（nil）与长度为零的列表都按空数组处理，
// 序列化为 [] 而非 null；首块前块标识为空字符串。
func TestBlockIDEmptyListForms(t *testing.T) {
	want := blockVectors()[0] // genesis-empty
	nilID := BlockID(1, 1, "", nil)
	emptyID := BlockID(1, 1, "", []string{})
	if nilID != want.id || emptyID != want.id {
		t.Fatalf("nil and zero-length lists must both yield the pinned empty-block id %s: nil=%s empty=%s",
			want.id, nilID, emptyID)
	}
	if nilID != emptyID {
		t.Fatalf("nil and zero-length lists differ: %s vs %s", nilID, emptyID)
	}
	// 空列表序列化为空数组，不是 null，也不是省略字段。
	if !strings.Contains(want.doc, `"transactions":[]`) {
		t.Fatalf("empty list must serialize as []: %s", want.doc)
	}
	nullSum := sha256.Sum256([]byte(`{"round":1,"height":1,"previous_id":"","transactions":null}`))
	if nilID == hex.EncodeToString(nullSum[:]) {
		t.Fatal("empty list must not serialize as null")
	}
	// 首块前块标识是空字符串，且空串与非空串产生不同标识。
	if got := BlockID(1, 1, blockVecTxA, nil); got == nilID {
		t.Fatal("empty previous id must differ from a non-empty one")
	}
}

// 交易列表保留给定顺序：同一对交易的两种相反顺序对应不同标识，
// 且生成标识时不对列表排序（传入已排序列表不会得到与未排序输入相同的结果）。
func TestBlockIDPreservesGivenOrder(t *testing.T) {
	orderA := BlockID(1, 1, "", []string{blockVecTxA, blockVecTxB})
	orderB := BlockID(1, 1, "", []string{blockVecTxB, blockVecTxA})
	if orderA == orderB {
		t.Fatal("reversing the transaction list must change the block id")
	}
	// blockVecTxA < blockVecTxB（字典序）：若实现内部排序，两种输入会得到同一结果。
	if blockVecTxA >= blockVecTxB {
		t.Fatal("test setup: vector tx ids must be in ascending lexicographic order")
	}
	vecs := blockVectors()
	if orderA != vecs[1].id || orderB != vecs[2].id {
		t.Fatalf("orders must match the pinned vectors: %s %s", orderA, orderB)
	}
	// 单个交易位置不同（列表更长时）同样影响标识。
	three := []string{blockVecTxA, blockVecTxB, blockVecTxA}
	moved := []string{blockVecTxA, blockVecTxA, blockVecTxB}
	if BlockID(1, 1, "", three) == BlockID(1, 1, "", moved) {
		t.Fatal("moving a transaction within the list must change the block id")
	}
}

// 轮次与高度是不同的输入：同轮不同高度、同高度不同轮次都必须改变标识，
// 防止把轮次当高度或反之。
func TestBlockIDRoundAndHeightDistinct(t *testing.T) {
	base := BlockID(7, 3, "", []string{blockVecTxA})
	if got := BlockID(3, 7, "", []string{blockVecTxA}); got == base {
		t.Fatal("swapping round and height must change the block id")
	}
	if got := BlockID(7, 4, "", []string{blockVecTxA}); got == base {
		t.Fatal("changing only the height must change the block id")
	}
	if got := BlockID(8, 3, "", []string{blockVecTxA}); got == base {
		t.Fatal("changing only the round must change the block id")
	}
	vecs := blockVectors()
	if base != vecs[3].id {
		t.Fatalf("round7-height3 = %s, want pinned %s", base, vecs[3].id)
	}
}

// 节点层面：两个账户各有一笔合法交易，同轮、同高度、同前块条件下两种相反
// 顺序对应不同候选标识；本地提议与按同一顺序登记的候选仍是同一个候选。
// 本地提议的标识同时与手工拼接文档的独立摘要核对，而非只与 BlockID 自比。
func TestBlockIDThroughProposalAndCandidates(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 打包规则：费用高者在前，本地提议顺序为 [b1, a1]。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	localOrder := []string{b1.ID(), a1.ID()}
	if fmt.Sprint(local.TxIDs) != fmt.Sprint(localOrder) {
		t.Fatalf("setup: local proposal order = %v, want %v", local.TxIDs, localOrder)
	}

	// 独立核对：手工拼接规范文档并取 SHA-256，必须等于本地提议的标识。
	wantLocal := independentBlockDigest(1, 1, "", localOrder)
	if local.BlockID != wantLocal {
		t.Fatalf("local proposal id = %s, want independently computed %s", local.BlockID, wantLocal)
	}

	// 相反顺序的竞争候选：同轮、同高度、同前块，标识必须不同。
	altOrder := []string{a1.ID(), b1.ID()}
	alt, err := n.RegisterCandidate(1, altOrder)
	if err != nil {
		t.Fatal(err)
	}
	if alt.Existing {
		t.Fatal("reversed order must not collapse into the local candidate")
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("reversed transaction order must yield a different candidate id")
	}
	if want := independentBlockDigest(1, 1, "", altOrder); alt.BlockID != want {
		t.Fatalf("alt candidate id = %s, want independently computed %s", alt.BlockID, want)
	}

	// 按本地提议同一顺序登记的候选仍是同一个候选。
	dup, err := n.RegisterCandidate(1, append([]string{}, localOrder...))
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Existing || dup.BlockID != local.BlockID {
		t.Fatalf("same order must return the local candidate: %+v local=%s", dup, local.BlockID)
	}
	// 候选查询中两者并存且各自保持登记时的顺序。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(rc.Candidates))
	}
	seen := map[string][]string{}
	for _, c := range rc.Candidates {
		seen[c.BlockID] = c.TxIDs
	}
	if fmt.Sprint(seen[local.BlockID]) != fmt.Sprint(localOrder) {
		t.Fatalf("local candidate order changed: %v", seen[local.BlockID])
	}
	if fmt.Sprint(seen[alt.BlockID]) != fmt.Sprint(altOrder) {
		t.Fatalf("alt candidate order changed: %v", seen[alt.BlockID])
	}
}

// 竞争候选达到确认条件后：确认块与交易查询关联的块标识沿用该候选原有标识，
// 确认块中的交易顺序与胜出候选一致。
func TestBlockIDConfirmedBlockKeepsCandidateID(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	for _, tx := range []*Transaction{a1, b1} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	local, err := n.Propose() // 本地顺序 [b1, a1]
	if err != nil {
		t.Fatal(err)
	}
	altOrder := []string{a1.ID(), b1.ID()} // 与本地提议相反
	alt, err := n.RegisterCandidate(1, altOrder)
	if err != nil {
		t.Fatal(err)
	}
	if alt.BlockID == local.BlockID {
		t.Fatal("setup: alt candidate must differ from local")
	}

	// 竞争候选达到现有确认条件（4 名验证者中 3 票，严格超过 2/3）。
	confirmCandidate(t, n, keys, alt.BlockID)

	// 确认块沿用候选原有标识，交易顺序与胜出候选一致。
	blk, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID != alt.BlockID {
		t.Fatalf("confirmed block id = %s, want the candidate's own id %s", blk.ID, alt.BlockID)
	}
	if fmt.Sprint(blk.TxIDs) != fmt.Sprint(altOrder) {
		t.Fatalf("confirmed block order = %v, want winner order %v", blk.TxIDs, altOrder)
	}
	if blk.PreviousID != "" {
		t.Fatalf("genesis block previous id = %q, want empty", blk.PreviousID)
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != alt.BlockID {
		t.Fatalf("latest block = %+v, want id %s", latest, alt.BlockID)
	}

	// 交易查询关联的块标识同样是该候选原有标识。
	for _, id := range altOrder {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockID != alt.BlockID || info.BlockHeight != 1 {
			t.Fatalf("tx %s association wrong: %+v", id, info)
		}
	}

	// 确认历史中的标识仍等于按公开规则对胜出列表独立计算的摘要。
	if want := independentBlockDigest(1, 1, "", altOrder); blk.ID != want {
		t.Fatalf("confirmed block id = %s, want independently computed %s", blk.ID, want)
	}
}

// 空交易列表的本地提议得到钉住的首块标识；轮次与高度在节点层面不被混淆：
// 确认一块后空过一轮，下一轮提议使用轮次 3、高度 2，并与独立摘要核对。
func TestBlockIDEmptyProposalAndRoundHeightAtNode(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 空池提议：空交易列表按空数组处理，首块前块标识为空字符串。
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p1.Empty || len(p1.TxIDs) != 0 {
		t.Fatalf("setup: proposal should be empty, got %v", p1.TxIDs)
	}
	if want := blockVectors()[0].id; p1.BlockID != want {
		t.Fatalf("empty genesis proposal id = %s, want pinned %s", p1.BlockID, want)
	}
	confirmCandidate(t, n, keys, p1.BlockID)
	blk1, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk1.ID != p1.BlockID || blk1.PreviousID != "" || len(blk1.TxIDs) != 0 {
		t.Fatalf("genesis block wrong: %+v", blk1)
	}

	// 空过第 2 轮：高度停在 1，轮次前进到 3。
	if _, err := n.EndRound(); err != nil {
		t.Fatal(err)
	}
	if n.CurrentRound() != 3 || n.Height() != 1 {
		t.Fatalf("round=%d height=%d, want 3/1", n.CurrentRound(), n.Height())
	}

	// 第 3 轮提议：轮次 3、高度 2、接在首块之后，与独立摘要逐字段核对。
	p3, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want := independentBlockDigest(3, 2, blk1.ID, nil)
	if p3.BlockID != want {
		t.Fatalf("round-3 proposal id = %s, want independently computed %s", p3.BlockID, want)
	}
	// 若把轮次当高度（3,3）或高度当轮次（2,2... 实为 (2,3)），摘要必然不同。
	if p3.BlockID == independentBlockDigest(2, 3, blk1.ID, nil) {
		t.Fatal("round and height appear swapped in the proposal id")
	}
}

// independentBlockDigest 按 README 的公开规则手工拼接区块文档并取 SHA-256，
// 不经过 BlockID，用于在节点层面独立核对各入口输出的标识。
func independentBlockDigest(round, height uint64, prevID string, txIDs []string) string {
	var list strings.Builder
	for i, id := range txIDs {
		if i > 0 {
			list.WriteString(",")
		}
		list.WriteString(`"` + id + `"`)
	}
	doc := fmt.Sprintf(`{"round":%d,"height":%d,"previous_id":%q,"transactions":[%s]}`,
		round, height, prevID, list.String())
	sum := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(sum[:])
}
