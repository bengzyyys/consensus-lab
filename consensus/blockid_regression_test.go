package consensus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉住 README“稳定编码规则”中区块标识的确切结果，而不仅是两次调用相等：
// 文档按 round、height、previous_id、transactions 的次序写成无额外空白的 JSON，
// 轮次与高度是完整的无符号十进制整数，前块标识与交易标识是字符串，
// 对文档的 UTF-8 字节取 SHA-256 得到六十四位小写十六进制标识。
// 表中的预期文档与摘要由独立实现（非本包代码）按公开规则预先算出并固定，
// 即使 BlockID、Propose、RegisterCandidate 与确认路径一起换了编码方式，
// 这些常量也会失配并指出与公开约定不一致。

// 区块向量使用的两个交易标识取自交易编码回归向量（见 encoding_regression_test.go），
// 任何人都可以按公开规则重建这两笔交易并核对标识。
const (
	blockVecTxA = "87ba6a9dbb6b733b36ed8722ea272fa8dc447faf44e368ec6048ce5597099bb0"
	blockVecTxB = "efd76322ec3b683d1557a712896ca6f21dce9fb11a754b4eb5c96f2fecf9417f"
)

// pinnedGenesisEmptyBlockID 是首轮、高度 1、空前块、空交易列表的钉住区块标识。
const pinnedGenesisEmptyBlockID = "ee9d28f151582c08818731640c00cf0dcbe3f9cff6e1ed4d7256ea7565791b7b"

// pinnedChainedEmptyBlockID 是第 2 轮、高度 2、前块为 pinnedGenesisEmptyBlockID、
// 空交易列表的钉住区块标识。
const pinnedChainedEmptyBlockID = "5cea9cf22c294aa0784857833f1a3206803613427a1e431d5197e10f98c53d9c"

// blockVector 的 doc 是按公开规则手工拼接的规范文档，id 是其 SHA-256
// 十六进制摘要；两者都可脱离本包独立核对。
type blockVector struct {
	name   string
	round  uint64
	height uint64
	prevID string
	txIDs  []string
	doc    string
	id     string
}

func stableBlockVectors() []blockVector {
	return []blockVector{
		{
			name:   "genesis-empty",
			round:  1,
			height: 1,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":1,"height":1,"previous_id":"","transactions":[]}`,
			id:     pinnedGenesisEmptyBlockID,
		},
		{
			name:   "two-tx-forward",
			round:  1,
			height: 1,
			prevID: "",
			txIDs:  []string{blockVecTxA, blockVecTxB},
			doc:    `{"round":1,"height":1,"previous_id":"","transactions":["` + blockVecTxA + `","` + blockVecTxB + `"]}`,
			id:     "49ce76b1e1f4b57ec49264a6f62c5e73a192e99a35d2795077ccb9beb4b14e13",
		},
		{
			// 同一批交易顺序相反必须是另一个标识：生成标识时不得排序。
			name:   "two-tx-reversed",
			round:  1,
			height: 1,
			prevID: "",
			txIDs:  []string{blockVecTxB, blockVecTxA},
			doc:    `{"round":1,"height":1,"previous_id":"","transactions":["` + blockVecTxB + `","` + blockVecTxA + `"]}`,
			id:     "44361f03d26ab8ed8daead90bd1f3c3b6df1b4b80f138987f44e024275be8057",
		},
		{
			// 轮次与高度是两个独立字段：交换取值必须得到不同标识。
			name:   "round2-height1",
			round:  2,
			height: 1,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":2,"height":1,"previous_id":"","transactions":[]}`,
			id:     "769491ca1fc77e9e928a22fc9f17f71fcb6c30b7b3c7570331402bda193faee2",
		},
		{
			name:   "round1-height2",
			round:  1,
			height: 2,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":1,"height":2,"previous_id":"","transactions":[]}`,
			id:     "40f8069fc4a1598f7ee75155528917cff7a23b4ce56bdc098ec02f49b382c86e",
		},
		{
			// 轮次与高度同时取 uint64 最大值，必须保持完整十进制整数。
			name:   "uint64-max",
			round:  18446744073709551615,
			height: 18446744073709551615,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":18446744073709551615,"height":18446744073709551615,"previous_id":"","transactions":[]}`,
			id:     "1628562d5cbd0c87e3cad1b06a6a7dbb7a2c3c537e8334cbb3c1ac2ec8f85483",
		},
		{
			// 与最大值仅差 1：经浮点或截断处理的实现无法区分这两个输入。
			name:   "uint64-max-round-minus-one",
			round:  18446744073709551614,
			height: 18446744073709551615,
			prevID: "",
			txIDs:  []string{},
			doc:    `{"round":18446744073709551614,"height":18446744073709551615,"previous_id":"","transactions":[]}`,
			id:     "d47ea05b9f11dc772ea7910f9af04d3440ad00099d1d472d35682ec722181d3c",
		},
		{
			// 前块标识作为字符串进入文档（首块之后链接历史的情形）。
			name:   "with-previous",
			round:  5,
			height: 3,
			prevID: pinnedGenesisEmptyBlockID,
			txIDs:  []string{blockVecTxA},
			doc: `{"round":5,"height":3,"previous_id":"` + pinnedGenesisEmptyBlockID + `","transactions":["` + blockVecTxA + `"]}`,
			id:  "60264e2dc18537615a1c0211fad26ddb989cb1c274b3e34809bd1e6729cdddea",
		},
		{
			name:   "chained-empty",
			round:  2,
			height: 2,
			prevID: pinnedGenesisEmptyBlockID,
			txIDs:  []string{},
			doc:    `{"round":2,"height":2,"previous_id":"` + pinnedGenesisEmptyBlockID + `","transactions":[]}`,
			id:     pinnedChainedEmptyBlockID,
		},
	}
}

// canonicalBlockDoc 按 README 的公开规则在测试侧手工拼接规范区块文档：
// 字段固定为 round、height、previous_id、transactions 的次序，无任何多余空白，
// 交易列表保留给定顺序。本函数不经过被检查的实现。
func canonicalBlockDoc(round, height uint64, prevID string, txIDs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"round":%d,"height":%d,"previous_id":%q,"transactions":[`, round, height, prevID)
	for i, id := range txIDs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q", id)
	}
	b.WriteString("]}")
	return b.String()
}

// 每个钉住向量做三重核对：BlockID 的输出等于钉住摘要；测试侧按公开规则
// 手工拼接的文档与钉住文档逐字节一致；钉住文档的 SHA-256 等于钉住摘要。
// 字段次序、整数表示、交易列表次序或摘要形式任何一项偏离都会在这里失败。
func TestBlockIDExactVectors(t *testing.T) {
	for _, v := range stableBlockVectors() {
		t.Run(v.name, func(t *testing.T) {
			got := BlockID(v.round, v.height, v.prevID, v.txIDs)

			// 钉住摘要结果。
			if got != v.id {
				t.Fatalf("BlockID = %s, want pinned %s", got, v.id)
			}
			// 小写 64 位十六进制。
			if len(got) != 64 || got != strings.ToLower(got) {
				t.Fatalf("block id must be 64 lowercase hex chars, got %q", got)
			}
			if _, err := hex.DecodeString(got); err != nil {
				t.Fatalf("block id is not valid hex: %v", err)
			}

			// 测试侧按公开规则重建的文档必须与钉住文档逐字节一致。
			doc := canonicalBlockDoc(v.round, v.height, v.prevID, v.txIDs)
			if doc != v.doc {
				t.Fatalf("canonical doc mismatch:\n got %s\nwant %s", doc, v.doc)
			}
			// 钉住文档的摘要必须等于钉住标识（文档与标识互相锚定）。
			sum := sha256.Sum256([]byte(v.doc))
			if hex.EncodeToString(sum[:]) != v.id {
				t.Fatalf("pinned doc digests to %x, want pinned id %s", sum, v.id)
			}

			// 文档内不得出现任何空白。
			if strings.ContainsAny(v.doc, " \t\r\n") {
				t.Fatalf("canonical doc must contain no whitespace: %s", v.doc)
			}
			// 字段次序：round、height、previous_id、transactions。
			if order := jsonKeyOrder(t, []byte(v.doc)); strings.Join(order, ",") !=
				"round,height,previous_id,transactions" {
				t.Fatalf("unexpected field order: %v", order)
			}
			// 轮次与高度是无符号整数的十进制表示：不带引号、小数点或指数。
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(v.doc), &fields); err != nil {
				t.Fatal(err)
			}
			for name, num := range map[string]uint64{"round": v.round, "height": v.height} {
				raw := string(fields[name])
				if raw != strconv.FormatUint(num, 10) {
					t.Fatalf("field %s encoded as %q, want plain decimal %d", name, raw, num)
				}
				if raw[0] == '"' || strings.ContainsAny(raw, ".eE+") {
					t.Fatalf("field %s must be a bare JSON integer, got %q", name, raw)
				}
			}
			// 前块标识与交易标识都是 JSON 字符串。
			var view struct {
				PreviousID   string   `json:"previous_id"`
				Transactions []string `json:"transactions"`
			}
			if err := json.Unmarshal([]byte(v.doc), &view); err != nil {
				t.Fatal(err)
			}
			if view.PreviousID != v.prevID {
				t.Fatalf("previous_id = %q, want %q", view.PreviousID, v.prevID)
			}
			if len(view.Transactions) != len(v.txIDs) {
				t.Fatalf("transactions = %v, want %v", view.Transactions, v.txIDs)
			}
			for i, id := range v.txIDs {
				if view.Transactions[i] != id {
					t.Fatalf("transactions[%d] = %q, want %q (order must be preserved)",
						i, view.Transactions[i], id)
				}
			}

			// 相同输入再次调用得到相同结果。
			if again := BlockID(v.round, v.height, v.prevID, v.txIDs); again != got {
				t.Fatalf("repeated call gave %s, want %s", again, got)
			}
		})
	}
}

// 未提供交易列表（nil）与长度为零的列表都按空数组处理，标识相同且等于
// 钉住的空块标识；文档中必须是 [] 而不是 null。
func TestBlockIDEmptyListForms(t *testing.T) {
	fromNil := BlockID(1, 1, "", nil)
	fromEmpty := BlockID(1, 1, "", []string{})
	if fromNil != fromEmpty {
		t.Fatalf("nil and empty tx list must share id: %s vs %s", fromNil, fromEmpty)
	}
	if fromNil != pinnedGenesisEmptyBlockID {
		t.Fatalf("empty block id = %s, want pinned %s", fromNil, pinnedGenesisEmptyBlockID)
	}
	if doc := canonicalBlockDoc(1, 1, "", nil); !strings.Contains(doc, `"transactions":[]`) {
		t.Fatalf("empty list must encode as []: %s", doc)
	}
	// 空数组与 null 的摘要不同：若实现把空列表编码成 null，会得到另一个标识，
	// 这里反向确认钉住标识对应的确实是空数组形式。
	nullDoc := `{"round":1,"height":1,"previous_id":"","transactions":null}`
	nullSum := sha256.Sum256([]byte(nullDoc))
	if fromNil == hex.EncodeToString(nullSum[:]) {
		t.Fatal("empty block id must hash an empty array, not null")
	}
}

// 交易列表顺序、轮次与高度的取值、以及 uint64 高位都必须参与标识：
// 任何归一化（排序、交换、截断）都会改变结果。
func TestBlockIDDistinguishingInputs(t *testing.T) {
	const maxUint = ^uint64(0)

	forward := BlockID(1, 1, "", []string{blockVecTxA, blockVecTxB})
	reversed := BlockID(1, 1, "", []string{blockVecTxB, blockVecTxA})
	if forward == reversed {
		t.Fatal("transaction order must affect the block id (no sorting)")
	}
	// 两个方向分别等于各自的钉住值：只对调后相等还不够，必须各自符合公开规则。
	if forward != "49ce76b1e1f4b57ec49264a6f62c5e73a192e99a35d2795077ccb9beb4b14e13" {
		t.Fatalf("forward order id = %s, want pinned", forward)
	}
	if reversed != "44361f03d26ab8ed8daead90bd1f3c3b6df1b4b80f138987f44e024275be8057" {
		t.Fatalf("reversed order id = %s, want pinned", reversed)
	}

	// 轮次与高度是不同的字段：互换取值必须得到不同标识，防止把轮次当高度。
	r2h1 := BlockID(2, 1, "", nil)
	r1h2 := BlockID(1, 2, "", nil)
	if r2h1 == r1h2 {
		t.Fatal("round and height must be distinct fields")
	}
	if r2h1 != "769491ca1fc77e9e928a22fc9f17f71fcb6c30b7b3c7570331402bda193faee2" {
		t.Fatalf("round2/height1 id = %s, want pinned", r2h1)
	}
	if r1h2 != "40f8069fc4a1598f7ee75155528917cff7a23b4ce56bdc098ec02f49b382c86e" {
		t.Fatalf("round1/height2 id = %s, want pinned", r1h2)
	}

	// uint64 最大值保持整数精度：与 max-1 必须得到不同标识（两个字段分别验证）。
	if BlockID(maxUint, maxUint, "", nil) == BlockID(maxUint-1, maxUint, "", nil) {
		t.Fatal("round must keep full uint64 precision")
	}
	if BlockID(maxUint, maxUint, "", nil) == BlockID(maxUint, maxUint-1, "", nil) {
		t.Fatal("height must keep full uint64 precision")
	}
	// 最大值的文档必须包含完整二十位十进制，而不是浮点或截断形式。
	maxDoc := canonicalBlockDoc(maxUint, maxUint, "", nil)
	if !strings.Contains(maxDoc, `"round":18446744073709551615`) ||
		!strings.Contains(maxDoc, `"height":18446744073709551615`) {
		t.Fatalf("uint64 max must be the full decimal in the doc: %s", maxDoc)
	}
	if BlockID(maxUint, maxUint, "", nil) != "1628562d5cbd0c87e3cad1b06a6a7dbb7a2c3c537e8334cbb3c1ac2ec8f85483" {
		t.Fatal("uint64 max block id drifted from pinned value")
	}

	// 前块标识参与摘要：空串与非空串不同。
	if BlockID(1, 1, "", nil) == BlockID(1, 1, pinnedGenesisEmptyBlockID, nil) {
		t.Fatal("previous_id must affect the block id")
	}
}

// 新节点的首个本地提议就是钉住的创世空块：轮次 1、高度 1、前块标识为空字符串、
// 空交易列表。Proposal 与 Propose 两个入口返回同一标识。
func TestBlockIDLocalProposalMatchesPinnedGenesis(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty || len(p.TxIDs) != 0 {
		t.Fatalf("empty pool must yield an empty proposal: %+v", p)
	}
	if p.BlockID != pinnedGenesisEmptyBlockID {
		t.Fatalf("genesis empty proposal id = %s, want pinned %s", p.BlockID, pinnedGenesisEmptyBlockID)
	}
	// 重复请求返回同一提议。
	p2, err := n.Propose()
	if err != nil || p2.BlockID != p.BlockID {
		t.Fatalf("repeated Propose changed the proposal: %+v (%v)", p2, err)
	}
	// Proposal 查询入口返回同一标识。
	view, ok := n.Proposal()
	if !ok || view.BlockID != pinnedGenesisEmptyBlockID {
		t.Fatalf("Proposal() = %+v, ok=%v, want pinned id %s", view, ok, pinnedGenesisEmptyBlockID)
	}

	// 未提供列表与空列表登记的候选都是同一个空块候选，且与本地提议相同。
	r1, err := n.RegisterCandidate(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.BlockID != pinnedGenesisEmptyBlockID || !r1.Existing {
		t.Fatalf("nil-list registration = %+v, want existing pinned empty block", r1)
	}
	r2, err := n.RegisterCandidate(1, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if r2.BlockID != pinnedGenesisEmptyBlockID || !r2.Existing {
		t.Fatalf("empty-list registration = %+v, want existing pinned empty block", r2)
	}
}

// 两个账户各有一笔合法交易：同轮、同高度、同前块条件下，两种相反顺序对应
// 不同候选标识；本地提议与按同一顺序登记的候选是同一个候选。
func TestBlockIDCandidateOrderAndIdentity(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	// 费用不同使本地提议顺序确定：a（费 10）在前，b（费 5）在后。
	a := NewTransaction(keys[0].priv, 1, []byte("a"), 10, 100)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	for _, tx := range []*Transaction{a, b} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{a.ID(), b.ID()}
	if len(p.TxIDs) != 2 || p.TxIDs[0] != wantOrder[0] || p.TxIDs[1] != wantOrder[1] {
		t.Fatalf("local proposal order = %v, want %v", p.TxIDs, wantOrder)
	}
	// 本地提议标识必须符合公开规则（测试侧独立重建文档核对）。
	if want := digestOfDoc(t, 1, 1, "", wantOrder); p.BlockID != want {
		t.Fatalf("local proposal id = %s, independent rule gives %s", p.BlockID, want)
	}

	// 相反顺序登记出另一个候选：标识不同，且同样符合公开规则。
	revOrder := []string{b.ID(), a.ID()}
	rev, err := n.RegisterCandidate(1, revOrder)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Existing {
		t.Fatal("reversed order must be a new candidate, not the local proposal")
	}
	if rev.BlockID == p.BlockID {
		t.Fatal("reversed transaction order must yield a different candidate id")
	}
	if want := digestOfDoc(t, 1, 1, "", revOrder); rev.BlockID != want {
		t.Fatalf("reversed candidate id = %s, independent rule gives %s", rev.BlockID, want)
	}

	// 按本地提议同一顺序登记：返回同一候选（同一标识），不新建候选。
	same, err := n.RegisterCandidate(1, wantOrder)
	if err != nil {
		t.Fatal(err)
	}
	if !same.Existing || same.BlockID != p.BlockID {
		t.Fatalf("same-order registration = %+v, want existing local proposal %s", same, p.BlockID)
	}
	// 重复登记相反顺序也返回同一候选。
	revAgain, err := n.RegisterCandidate(1, revOrder)
	if err != nil || !revAgain.Existing || revAgain.BlockID != rev.BlockID {
		t.Fatalf("repeated reversed registration = %+v (%v), want existing %s", revAgain, err, rev.BlockID)
	}

	// 本地提议不受其他候选登记影响。
	view, ok := n.Proposal()
	if !ok || view.BlockID != p.BlockID {
		t.Fatalf("local proposal changed after registrations: %+v", view)
	}
}

// 竞争候选达到现有确认条件（票数严格超过名单 2/3）后，确认块与交易查询关联的
// 块标识必须沿用该候选原有标识，确认块中的交易顺序与胜出候选一致。
func TestBlockIDConfirmedBlockKeepsCandidateID(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a := NewTransaction(keys[0].priv, 1, []byte("a"), 10, 100)
	b := NewTransaction(keys[1].priv, 1, []byte("b"), 5, 100)
	for _, tx := range []*Transaction{a, b} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Propose(); err != nil {
		t.Fatal(err)
	}

	// 竞争候选采用与本地提议相反的顺序。
	winnerOrder := []string{b.ID(), a.ID()}
	cand, err := n.RegisterCandidate(1, winnerOrder)
	if err != nil {
		t.Fatal(err)
	}

	// 4 名验证者需 3 票确认；逐票投给竞争候选。
	var confirmed *Block
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, cand.BlockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
		if i < 2 && res.Confirmed {
			t.Fatalf("confirmed too early at vote %d", i+1)
		}
		if i == 2 {
			if !res.Confirmed || res.Block == nil {
				t.Fatal("third vote must confirm the candidate")
			}
			confirmed = res.Block
		}
	}

	// 确认块沿用候选原有标识，交易顺序与胜出候选一致。
	if confirmed.ID != cand.BlockID {
		t.Fatalf("confirmed block id = %s, want candidate id %s", confirmed.ID, cand.BlockID)
	}
	if confirmed.Height != 1 || confirmed.Round != 1 || confirmed.PreviousID != "" {
		t.Fatalf("confirmed block = %+v, want height 1 round 1 empty previous id", confirmed)
	}
	if len(confirmed.TxIDs) != 2 || confirmed.TxIDs[0] != winnerOrder[0] || confirmed.TxIDs[1] != winnerOrder[1] {
		t.Fatalf("confirmed tx order = %v, want winner order %v", confirmed.TxIDs, winnerOrder)
	}

	// 高度查询与最新块查询返回同一标识与顺序。
	at, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if at.ID != cand.BlockID || at.TxIDs[0] != winnerOrder[0] || at.TxIDs[1] != winnerOrder[1] {
		t.Fatalf("BlockAt(1) = %+v, want id %s with winner order", at, cand.BlockID)
	}
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != cand.BlockID {
		t.Fatalf("LatestBlock = %+v, ok=%v, want id %s", latest, ok, cand.BlockID)
	}

	// 交易查询关联的块标识是该候选原有标识。
	for _, id := range winnerOrder {
		info, err := n.Tx(id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockID != cand.BlockID || info.BlockHeight != 1 {
			t.Fatalf("Tx(%s) = %+v, want confirmed in block %s at height 1", id, info, cand.BlockID)
		}
	}

	// 候选查询中胜出者保留原标识与原交易顺序。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	var winnerFound bool
	for _, c := range rc.Candidates {
		if c.BlockID == cand.BlockID {
			winnerFound = true
			if c.Result != CandidateWon {
				t.Fatalf("winner candidate result = %s, want won", c.Result)
			}
			if len(c.TxIDs) != 2 || c.TxIDs[0] != winnerOrder[0] || c.TxIDs[1] != winnerOrder[1] {
				t.Fatalf("winner candidate tx order = %v, want %v", c.TxIDs, winnerOrder)
			}
		}
	}
	if !winnerFound {
		t.Fatalf("winner candidate %s missing from round 1 records", cand.BlockID)
	}

	// 进入下一轮：新提议接在确认块之后（高度 2、前块为胜出候选标识），
	// 标识仍按公开规则计算。
	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if want := digestOfDoc(t, 2, 2, cand.BlockID, nil); p2.BlockID != want {
		t.Fatalf("round-2 proposal id = %s, independent rule gives %s", p2.BlockID, want)
	}
}

// 空块经投票确认后，下一轮空提议的标识等于钉住的链式空块标识：
// 轮次 2、高度 2、前块为钉住的创世空块标识、空交易列表。
func TestBlockIDChainedEmptyBlocks(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if p.BlockID != pinnedGenesisEmptyBlockID {
		t.Fatalf("genesis proposal id = %s, want pinned %s", p.BlockID, pinnedGenesisEmptyBlockID)
	}
	confirmByVotes(t, n, keys, p.BlockID)

	// 确认块本身也沿用钉住标识。
	latest, ok := n.LatestBlock()
	if !ok || latest.ID != pinnedGenesisEmptyBlockID || latest.Height != 1 || latest.PreviousID != "" {
		t.Fatalf("confirmed genesis block = %+v, ok=%v, want pinned id %s", latest, ok, pinnedGenesisEmptyBlockID)
	}
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}

	p2, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if p2.BlockID != pinnedChainedEmptyBlockID {
		t.Fatalf("chained empty proposal id = %s, want pinned %s", p2.BlockID, pinnedChainedEmptyBlockID)
	}
}

// digestOfDoc 在测试侧按公开规则独立计算区块标识：手工拼接规范文档后取
// SHA-256 十六进制摘要，不经过被检查的 BlockID 入口。
func digestOfDoc(t *testing.T, round, height uint64, prevID string, txIDs []string) string {
	t.Helper()
	doc := canonicalBlockDoc(round, height, prevID, txIDs)
	sum := sha256.Sum256([]byte(doc))
	return hex.EncodeToString(sum[:])
}
