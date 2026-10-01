package consensus

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
)

func genValidators(t *testing.T, n int) ([][]byte, []ed25519.PrivateKey) {
	t.Helper()
	vals := make([][]byte, n)
	privs := make([]ed25519.PrivateKey, n)
	for i := 0; i < n; i++ {
		pub, priv, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		vals[i] = append([]byte(nil), pub...)
		privs[i] = priv
	}
	return vals, privs
}

func openTest(t *testing.T, seed int64, vals [][]byte, limit uint64) *Node {
	t.Helper()
	node, err := Open(t.TempDir(), Config{Seed: seed, Validators: vals, TxLimit: limit})
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func makeTx(t *testing.T, priv ed25519.PrivateKey, sender []byte, seq, fee, expiry uint64, content string) Tx {
	t.Helper()
	tx := Tx{
		Sender:  append([]byte(nil), sender...),
		Seq:     seq,
		Content: []byte(content),
		Fee:     fee,
		Expiry:  expiry,
	}
	SignTx(priv, &tx)
	return tx
}

func txIDHex(tx *Tx) string {
	id := TxIDOf(tx)
	return EncodeHex(id[:])
}

// 初始状态：轮次 1、无确认块、出块者可查。
func TestInitialState(t *testing.T) {
	vals, _ := genValidators(t, 4)
	node := openTest(t, 42, vals, 3)
	if node.Round() != 1 {
		t.Fatalf("round = %d, want 1", node.Round())
	}
	if node.Height() != 0 {
		t.Fatalf("height = %d, want 0", node.Height())
	}
	p := node.Proposer()
	if len(p) != 32 {
		t.Fatalf("proposer length = %d", len(p))
	}
	if _, err := node.Block(1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("block 1 err = %v, want ErrNotFound", err)
	}
}

// 配置校验：空名单、重复验证者、非正上限。
func TestOpenValidation(t *testing.T) {
	vals, _ := genValidators(t, 2)
	if _, err := Open(t.TempDir(), Config{Seed: 1, Validators: nil, TxLimit: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty validators err = %v, want ErrInvalidConfig", err)
	}
	dup := [][]byte{vals[0], vals[0]}
	if _, err := Open(t.TempDir(), Config{Seed: 1, Validators: dup, TxLimit: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate validators err = %v, want ErrInvalidConfig", err)
	}
	if _, err := Open(t.TempDir(), Config{Seed: 1, Validators: vals, TxLimit: 0}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero limit err = %v, want ErrInvalidConfig", err)
	}
	bad := [][]byte{vals[0][:31]}
	if _, err := Open(t.TempDir(), Config{Seed: 1, Validators: bad, TxLimit: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad key err = %v, want ErrInvalidConfig", err)
	}
}

// 提交校验：合法入池；非法签名、到期、序号过低、重复均拒绝并返回具体原因。
func TestSubmitRejections(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	sender := vals[0]
	priv := privs[0]

	tx := makeTx(t, priv, sender, 1, 10, 5, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatalf("submit valid tx: %v", err)
	}
	info, err := node.TxStatus(TxIDOf(&tx))
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("status = %s, want queued", info.Status)
	}

	// 非法签名：篡改内容后签名不匹配。
	bad := makeTx(t, priv, sender, 2, 10, 5, "b")
	bad.Content[0] = 'x'
	if _, err := node.SubmitTx(&bad); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("bad signature err = %v, want ErrInvalidSignature", err)
	}

	// 到期：到期轮次不大于当前轮次（1）。
	exp := makeTx(t, priv, sender, 3, 10, 1, "c")
	if _, err := node.SubmitTx(&exp); !errors.Is(err, ErrTxExpired) {
		t.Fatalf("expired err = %v, want ErrTxExpired", err)
	}

	// 序号过低：不大于已确认序号（0）——序号 0 本身非法。
	zero := Tx{Sender: sender, Seq: 0, Content: []byte("z"), Fee: 1, Expiry: 5}
	SignTx(priv, &zero)
	if _, err := node.SubmitTx(&zero); !errors.Is(err, ErrInvalidSeq) {
		t.Fatalf("zero seq err = %v, want ErrInvalidSeq", err)
	}

	// 重复交易：同一笔交易再次提交。
	dup := makeTx(t, priv, sender, 1, 10, 5, "a")
	if _, err := node.SubmitTx(&dup); !errors.Is(err, ErrDuplicateTx) {
		t.Fatalf("duplicate err = %v, want ErrDuplicateTx", err)
	}

	// 序号有缺口的交易可以入池等待。
	gap := makeTx(t, priv, sender, 5, 10, 5, "gap")
	if _, err := node.SubmitTx(&gap); err != nil {
		t.Fatalf("gap tx submit: %v", err)
	}
}

// 费用替换：同发送者同序号只保留一笔，费用严格更高才可替换，
// 旧交易可查询到已替换及新交易标识。
func TestFeeReplacement(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	sender := vals[0]
	priv := privs[0]

	tx1 := makeTx(t, priv, sender, 1, 10, 5, "low")
	id1, err := node.SubmitTx(&tx1)
	if err != nil {
		t.Fatal(err)
	}

	// 费用不高于现有交易，拒绝。
	same := makeTx(t, priv, sender, 1, 10, 5, "same-fee")
	if _, err := node.SubmitTx(&same); !errors.Is(err, ErrFeeNotHigher) {
		t.Fatalf("same fee err = %v, want ErrFeeNotHigher", err)
	}
	lower := makeTx(t, priv, sender, 1, 5, 5, "lower")
	if _, err := node.SubmitTx(&lower); !errors.Is(err, ErrFeeNotHigher) {
		t.Fatalf("lower fee err = %v, want ErrFeeNotHigher", err)
	}

	// 费用严格更高，替换成功。
	tx2 := makeTx(t, priv, sender, 1, 20, 5, "high")
	id2, err := node.SubmitTx(&tx2)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if id1 == id2 {
		t.Fatal("replacement tx has same id")
	}
	old, err := node.TxStatus(id1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != StatusReplaced {
		t.Fatalf("old status = %s, want replaced", old.Status)
	}
	if old.ReplacedBy != id2 {
		t.Fatalf("old.ReplacedBy = %x, want %x", old.ReplacedBy, id2)
	}
	newInfo, err := node.TxStatus(id2)
	if err != nil {
		t.Fatal(err)
	}
	if newInfo.Status != StatusQueued {
		t.Fatalf("new status = %s, want queued", newInfo.Status)
	}

	// 替换后再提交原交易（ID 不同）仍可，因为原交易已被替换出池。
	tx3 := makeTx(t, priv, sender, 1, 30, 5, "higher")
	if _, err := node.SubmitTx(&tx3); err != nil {
		t.Fatalf("higher replacement: %v", err)
	}
}

// 已提议交易禁止替换。
func TestProposedTxCannotBeReplaced(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	sender := vals[0]
	priv := privs[0]

	tx := makeTx(t, priv, sender, 1, 10, 5, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Propose(); err != nil {
		t.Fatal(err)
	}
	rep := makeTx(t, priv, sender, 1, 99, 5, "b")
	if _, err := node.SubmitTx(&rep); !errors.Is(err, ErrTxProposed) {
		t.Fatalf("replace proposed err = %v, want ErrTxProposed", err)
	}
}

// 打包：费用优先，同费用按交易标识字典序，选入后继续考虑账户后续序号，
// 直到上限；没有可选交易时返回空块。
func TestPackingOrder(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	a, pa := vals[0], privs[0]
	b, pb := vals[1], privs[1]

	// A: seq1 fee10, seq2 fee5；B: seq1 fee20。
	// 期望顺序：B1(20)、A1(10)、A2(5)。
	txA1 := makeTx(t, pa, a, 1, 10, 5, "a1")
	txA2 := makeTx(t, pa, a, 2, 5, 5, "a2")
	txB1 := makeTx(t, pb, b, 1, 20, 5, "b1")
	for _, tx := range []*Tx{&txA1, &txA2, &txB1} {
		if _, err := node.SubmitTx(tx); err != nil {
			t.Fatal(err)
		}
	}
	blk, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	want := []TxID{TxIDOf(&txB1), TxIDOf(&txA1), TxIDOf(&txA2)}
	if len(blk.TxIDs) != len(want) {
		t.Fatalf("tx count = %d, want %d", len(blk.TxIDs), len(want))
	}
	for i := range want {
		if blk.TxIDs[i] != want[i] {
			t.Fatalf("tx[%d] = %x, want %x", i, blk.TxIDs[i], want[i])
		}
	}
	if blk.Height != 1 || blk.Round != 1 {
		t.Fatalf("block height/round = %d/%d", blk.Height, blk.Round)
	}
}

// 同费用按交易标识字典序选择。
func TestPackingTieBreak(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 2)
	var ids []TxID
	for i := 0; i < 3; i++ {
		tx := makeTx(t, privs[i], vals[i], 1, 10, 5, fmt.Sprintf("tie-%d", i))
		id, err := node.SubmitTx(&tx)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	blk, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	// 两个位置应是 ids 中字典序最小的两个。
	sorted := append([]TxID(nil), ids...)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if bytes.Compare(sorted[i][:], sorted[j][:]) > 0 {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	want := sorted[:2]
	for i := range want {
		if blk.TxIDs[i] != want[i] {
			t.Fatalf("tx[%d] = %x, want %x", i, blk.TxIDs[i], want[i])
		}
	}
}

// 没有可选交易时返回空块。
func TestEmptyBlock(t *testing.T) {
	vals, _ := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	blk, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if len(blk.TxIDs) != 0 {
		t.Fatalf("empty block has %d txs", len(blk.TxIDs))
	}
	if blk.Height != 1 || blk.Round != 1 {
		t.Fatalf("block height/round = %d/%d", blk.Height, blk.Round)
	}
}

// 每轮只有一个提议，重复请求返回同一提议；此后提交的交易不改变当前提议。
func TestProposalIdempotent(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	tx := makeTx(t, privs[0], vals[0], 1, 10, 5, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatal(err)
	}
	b1, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if b1.ID() != b2.ID() {
		t.Fatal("repeated Propose returned different blocks")
	}
	// 提议后提交的交易不进入当前提议。
	tx2 := makeTx(t, privs[1], vals[1], 1, 99, 5, "b")
	if _, err := node.SubmitTx(&tx2); err != nil {
		t.Fatal(err)
	}
	b3, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if b3.ID() != b1.ID() {
		t.Fatal("post-proposal submission changed proposal")
	}
	if len(b3.TxIDs) != 1 || b3.TxIDs[0] != TxIDOf(&tx) {
		t.Fatalf("proposal txs = %v, want only first tx", b3.TxIDs)
	}
}

// 投票阈值：4 个验证者需严格超过 2/3（至少 3 票）才立即确认；
// 重复投同一票不增加票数。
func TestVoteThresholdAndDuplicate(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	tx := makeTx(t, privs[0], vals[0], 1, 10, 5, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatal(err)
	}
	blk, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	blockID := blk.ID()

	c1, err := node.Vote(vals[0], 1, blockID)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != 1 {
		t.Fatalf("count = %d, want 1", c1)
	}
	if node.Round() != 1 {
		t.Fatalf("round advanced after 1 vote")
	}
	c2, err := node.Vote(vals[1], 1, blockID)
	if err != nil {
		t.Fatal(err)
	}
	if c2 != 2 {
		t.Fatalf("count = %d, want 2", c2)
	}
	if node.Round() != 1 {
		t.Fatalf("round advanced after 2 votes")
	}
	// 重复投票不增加票数。
	if _, err := node.Vote(vals[1], 1, blockID); !errors.Is(err, ErrDuplicateVote) {
		t.Fatalf("duplicate vote err = %v, want ErrDuplicateVote", err)
	}
	c3, err := node.Vote(vals[2], 1, blockID)
	if err != nil {
		t.Fatal(err)
	}
	if c3 != 3 {
		t.Fatalf("count = %d, want 3", c3)
	}
	// 严格超过 2/3：3/4 > 2/3，立即确认并进入下一轮。
	if node.Round() != 2 {
		t.Fatalf("round = %d, want 2 after confirm", node.Round())
	}
	if node.Height() != 1 {
		t.Fatalf("height = %d, want 1", node.Height())
	}
	confirmed, err := node.Block(1)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.ID() != blockID {
		t.Fatal("confirmed block id mismatch")
	}
	if confirmed.PrevHash != (Hash{}) {
		t.Fatal("genesis prev hash not zero")
	}
}

// 3 个验证者时，2 票不达标、3 票达标（3/3 > 2/3）。
func TestVoteThresholdThreeValidators(t *testing.T) {
	vals, _ := genValidators(t, 3)
	node := openTest(t, 1, vals, 3)
	prop, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	blockID := prop.ID()
	if _, err := node.Vote(vals[0], 1, blockID); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Vote(vals[1], 1, blockID); err != nil {
		t.Fatal(err)
	}
	if node.Round() != 1 {
		t.Fatalf("round = %d, want 1 after 2/3 votes", node.Round())
	}
	if _, err := node.Vote(vals[2], 1, blockID); err != nil {
		t.Fatal(err)
	}
	if node.Round() != 2 {
		t.Fatalf("round = %d, want 2 after 3/3 votes", node.Round())
	}
}

// 投票拒绝：名单外身份、错误轮次、错误区块标识。
func TestVoteRejections(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	if _, err := node.Propose(); err != nil {
		t.Fatal(err)
	}
	prop, _ := node.Propose()
	blockID := prop.ID()

	outsider, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Vote(outsider, 1, blockID); !errors.Is(err, ErrNotValidator) {
		t.Fatalf("outsider err = %v, want ErrNotValidator", err)
	}
	if _, err := node.Vote(vals[0], 2, blockID); !errors.Is(err, ErrWrongRound) {
		t.Fatalf("wrong round err = %v, want ErrWrongRound", err)
	}
	var wrong Hash
	wrong[0] = 1
	if _, err := node.Vote(vals[0], 1, wrong); !errors.Is(err, ErrWrongBlock) {
		t.Fatalf("wrong block err = %v, want ErrWrongBlock", err)
	}
	_ = privs
}

// 确认推进账户序号、移除已确认交易；后续块高度连续、前块标识链接。
func TestConfirmAdvancesState(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	a, pa := vals[0], privs[0]

	tx1 := makeTx(t, pa, a, 1, 10, 5, "a1")
	tx2 := makeTx(t, pa, a, 2, 10, 5, "a2")
	for _, tx := range []*Tx{&tx1, &tx2} {
		if _, err := node.SubmitTx(tx); err != nil {
			t.Fatal(err)
		}
	}
	b1, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := node.Vote(vals[i], 1, b1.ID()); err != nil {
			t.Fatal(err)
		}
	}
	acc, err := node.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ConfirmedSeq != 2 {
		t.Fatalf("confirmed seq = %d, want 2", acc.ConfirmedSeq)
	}
	if len(acc.Pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(acc.Pending))
	}
	s1, _ := node.TxStatus(TxIDOf(&tx1))
	if s1.Status != StatusConfirmed || s1.BlockHeight != 1 {
		t.Fatalf("tx1 status = %s height %d", s1.Status, s1.BlockHeight)
	}

	// 第二轮：提交 seq3，确认后高度连续、前块标识为第一块标识。
	tx3 := makeTx(t, pa, a, 3, 10, 5, "a3")
	if _, err := node.SubmitTx(&tx3); err != nil {
		t.Fatal(err)
	}
	b2, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if b2.Height != 2 || b2.Round != 2 {
		t.Fatalf("block2 height/round = %d/%d", b2.Height, b2.Round)
	}
	if b2.PrevHash != b1.ID() {
		t.Fatal("prev hash does not link block 1")
	}
	for i := 0; i < 3; i++ {
		if _, err := node.Vote(vals[i], 2, b2.ID()); err != nil {
			t.Fatal(err)
		}
	}
	if node.Height() != 2 || node.Round() != 3 {
		t.Fatalf("height/round = %d/%d, want 2/3", node.Height(), node.Round())
	}
}

// 主动结束未确认轮次：不留下确认块，原提议交易回到排队状态，轮次推进。
func TestEndRound(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	tx := makeTx(t, privs[0], vals[0], 1, 10, 5, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatal(err)
	}
	blk, err := node.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Vote(vals[0], 1, blk.ID()); err != nil {
		t.Fatal(err)
	}
	if err := node.EndRound(); err != nil {
		t.Fatal(err)
	}
	if node.Round() != 2 {
		t.Fatalf("round = %d, want 2", node.Round())
	}
	if node.Height() != 0 {
		t.Fatalf("height = %d, want 0 (no block)", node.Height())
	}
	if _, err := node.Block(1); !errors.Is(err, ErrNotFound) {
		t.Fatal("block 1 exists after end round")
	}
	info, err := node.TxStatus(TxIDOf(&tx))
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusQueued {
		t.Fatalf("status = %s, want queued after end round", info.Status)
	}
	// 结束后可以重新提议。
	if _, err := node.Propose(); err != nil {
		t.Fatal(err)
	}
}

// 进入新轮次时，到期轮次不大于当前轮次的池内交易失效。
func TestExpiry(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	tx := makeTx(t, privs[0], vals[0], 1, 10, 2, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatal(err)
	}
	if err := node.EndRound(); err != nil {
		t.Fatal(err)
	}
	if node.Round() != 2 {
		t.Fatalf("round = %d, want 2", node.Round())
	}
	info, err := node.TxStatus(TxIDOf(&tx))
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", info.Status)
	}
	acc, err := node.Account(vals[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(acc.Pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(acc.Pending))
	}
}

// 旧轮次投票不能影响新提议：确认后再投旧轮次被拒绝。
func TestOldRoundVoteRejected(t *testing.T) {
	vals, _ := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	if _, err := node.Propose(); err != nil {
		t.Fatal(err)
	}
	prop, _ := node.Propose()
	for i := 0; i < 3; i++ {
		if _, err := node.Vote(vals[i], 1, prop.ID()); err != nil {
			t.Fatal(err)
		}
	}
	// 已进入第 2 轮，旧轮次投票被拒绝。
	if _, err := node.Vote(vals[3], 1, prop.ID()); !errors.Is(err, ErrWrongRound) {
		t.Fatalf("old round vote err = %v, want ErrWrongRound", err)
	}
}

// 账户查询：已确认序号、待处理交易状态、最早缺口序号。
func TestAccountQuery(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	a, pa := vals[0], privs[0]

	// seq1 等待打包，seq3 等待打包（缺口 seq2）。
	tx1 := makeTx(t, pa, a, 1, 10, 5, "a1")
	tx3 := makeTx(t, pa, a, 3, 10, 5, "a3")
	for _, tx := range []*Tx{&tx1, &tx3} {
		if _, err := node.SubmitTx(tx); err != nil {
			t.Fatal(err)
		}
	}
	acc, err := node.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ConfirmedSeq != 0 {
		t.Fatalf("confirmed seq = %d, want 0", acc.ConfirmedSeq)
	}
	if !acc.HasGap || acc.Gap != 2 {
		t.Fatalf("gap = %d (hasGap=%v), want 2", acc.Gap, acc.HasGap)
	}
	if len(acc.Pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(acc.Pending))
	}
	for _, p := range acc.Pending {
		if p.Status != StatusQueued {
			t.Fatalf("pending status = %s, want queued", p.Status)
		}
	}

	// 提议后 seq1 等待投票。
	if _, err := node.Propose(); err != nil {
		t.Fatal(err)
	}
	acc2, err := node.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	statusBySeq := map[uint64]string{}
	for _, p := range acc2.Pending {
		statusBySeq[p.Seq] = p.Status
	}
	if statusBySeq[1] != StatusProposed {
		t.Fatalf("seq1 status = %s, want proposed", statusBySeq[1])
	}
	if statusBySeq[3] != StatusQueued {
		t.Fatalf("seq3 status = %s, want queued", statusBySeq[3])
	}
	// 提议后缺口仍为 seq2。
	if !acc2.HasGap || acc2.Gap != 2 {
		t.Fatalf("gap = %d (hasGap=%v), want 2", acc2.Gap, acc2.HasGap)
	}
}

// 持久化：停止再启动后恢复轮次、池、未决提议、票数与确认历史。
func TestPersistence(t *testing.T) {
	vals, privs := genValidators(t, 4)
	dir := t.TempDir()
	node, err := Open(dir, Config{Seed: 7, Validators: vals, TxLimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	tx := makeTx(t, privs[0], vals[0], 1, 10, 5, "a")
	if _, err := node.SubmitTx(&tx); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Propose(); err != nil {
		t.Fatal(err)
	}
	prop, _ := node.Propose()
	if _, err := node.Vote(vals[0], 1, prop.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Vote(vals[1], 1, prop.ID()); err != nil {
		t.Fatal(err)
	}

	// 重新打开同一目录。
	node2, err := Open(dir, Config{Seed: 7, Validators: vals, TxLimit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if node2.Round() != 1 {
		t.Fatalf("round = %d, want 1", node2.Round())
	}
	if node2.Height() != 0 {
		t.Fatalf("height = %d, want 0", node2.Height())
	}
	if _, err := node2.Vote(vals[0], 1, prop.ID()); !errors.Is(err, ErrDuplicateVote) {
		t.Fatalf("restored vote not counted: err = %v", err)
	}
	count, err := node2.Vote(vals[2], 1, prop.ID())
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if node2.Round() != 2 {
		t.Fatalf("round = %d, want 2 after confirm", node2.Round())
	}
	if node2.Height() != 1 {
		t.Fatalf("height = %d, want 1", node2.Height())
	}
	blk, err := node2.Block(1)
	if err != nil {
		t.Fatal(err)
	}
	if blk.ID() != prop.ID() {
		t.Fatal("restored block id mismatch")
	}
	// 恢复后重复提交仍被拒绝。
	if _, err := node2.SubmitTx(&tx); !errors.Is(err, ErrDuplicateTx) {
		t.Fatalf("duplicate after reload err = %v", err)
	}
	// 恢复后交易状态仍可查。
	info, err := node2.TxStatus(TxIDOf(&tx))
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed {
		t.Fatalf("status = %s, want confirmed", info.Status)
	}
}

// 确定性：种子、初始配置和输入顺序相同时，提议者、交易顺序、
// 区块标识和确认历史必须一致。
func TestDeterminism(t *testing.T) {
	vals, privs := genValidators(t, 5)
	seed := int64(123)
	limit := uint64(3)

	run := func() (*Node, []Hash) {
		node := openTest(t, seed, vals, limit)
		var blockIDs []Hash
		for round := 1; round <= 4; round++ {
			// 每轮提交若干交易。
			for i := 0; i < 2; i++ {
				idx := (round + i) % len(vals)
				seq := uint64(round*10 + i + 1)
				tx := makeTx(t, privs[idx], vals[idx], seq, uint64(100-i*10), 100, fmt.Sprintf("r%d-%d", round, i))
				if _, err := node.SubmitTx(&tx); err != nil {
					// 序号冲突时跳过（确定性下两次运行行为一致）。
					continue
				}
			}
			blk, err := node.Propose()
			if err != nil {
				t.Fatal(err)
			}
			blockIDs = append(blockIDs, blk.ID())
			for v := 0; v < 4; v++ {
				if _, err := node.Vote(vals[v], uint64(round), blk.ID()); err != nil {
					t.Fatal(err)
				}
			}
		}
		return node, blockIDs
	}

	n1, ids1 := run()
	n2, ids2 := run()
	if len(ids1) != len(ids2) {
		t.Fatalf("block id count = %d vs %d", len(ids1), len(ids2))
	}
	for i := range ids1 {
		if ids1[i] != ids2[i] {
			t.Fatalf("block %d id mismatch: %x vs %x", i+1, ids1[i], ids2[i])
		}
	}
	// 确认历史一致。
	for h := uint64(1); h <= 4; h++ {
		b1, _ := n1.Block(h)
		b2, _ := n2.Block(h)
		if b1.ID() != b2.ID() {
			t.Fatalf("confirmed block %d id mismatch", h)
		}
		if len(b1.TxIDs) != len(b2.TxIDs) {
			t.Fatalf("confirmed block %d tx count mismatch", h)
		}
		for i := range b1.TxIDs {
			if b1.TxIDs[i] != b2.TxIDs[i] {
				t.Fatalf("confirmed block %d tx %d mismatch", h, i)
			}
		}
	}
}

// 不同种子产生不同的出块者顺序。
func TestDifferentSeedDifferentProposer(t *testing.T) {
	vals, _ := genValidators(t, 5)
	n1 := openTest(t, 1, vals, 3)
	n2 := openTest(t, 2, vals, 3)
	p1 := n1.Proposer()
	p2 := n2.Proposer()
	// 不同种子下首轮出块者可能相同，但多轮顺序应不同；
	// 这里验证洗牌顺序确实被种子改变（至少有一轮不同）。
	diff := false
	for i := 0; i < 5; i++ {
		if !bytes.Equal(n1.Proposer(), n2.Proposer()) {
			diff = true
		}
		_ = n1.EndRound()
		_ = n2.EndRound()
	}
	if !diff {
		t.Fatal("different seeds produced identical proposer sequence")
	}
	_ = p1
	_ = p2
}

// 确认历史不得改写：已确认块高度连续且仅追加。
func TestHistoryAppendOnly(t *testing.T) {
	vals, privs := genValidators(t, 4)
	node := openTest(t, 1, vals, 3)
	for round := 1; round <= 3; round++ {
		tx := makeTx(t, privs[round-1], vals[round-1], 1, 10, 100, fmt.Sprintf("r%d", round))
		if _, err := node.SubmitTx(&tx); err != nil {
			t.Fatal(err)
		}
		blk, err := node.Propose()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			if _, err := node.Vote(vals[i], uint64(round), blk.ID()); err != nil {
				t.Fatal(err)
			}
		}
	}
	for h := uint64(1); h <= 3; h++ {
		b, err := node.Block(h)
		if err != nil {
			t.Fatal(err)
		}
		if b.Height != h {
			t.Fatalf("block %d has height %d", h, b.Height)
		}
	}
	if node.Height() != 3 {
		t.Fatalf("height = %d, want 3", node.Height())
	}
}
