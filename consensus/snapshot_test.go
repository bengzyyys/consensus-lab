package consensus

import (
	"fmt"
	"testing"
)

// setupTwoTxNode 准备一个含两笔交易的新节点：tx1（keys[0]，费用 1）、
// tx2（keys[1]，费用 2）。本地提议按费用打包，顺序固定为 [tx2, tx1]。
func setupTwoTxNode(t *testing.T) (*Node, string, []testKey, *Transaction, *Transaction) {
	t.Helper()
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	tx2 := NewTransaction(keys[1].priv, 1, []byte("b"), 2, 100)
	if _, err := n.Submit(tx1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(tx2); err != nil {
		t.Fatal(err)
	}
	return n, dir, keys, tx1, tx2
}

// assertBlock 校验确认块的交易列表、高度、轮次、区块标识与前块标识。
func assertBlock(t *testing.T, b Block, wantTx []string, height, round uint64, id, prev string) {
	t.Helper()
	if fmt.Sprint(b.TxIDs) != fmt.Sprint(wantTx) {
		t.Fatalf("block tx ids = %v, want %v", b.TxIDs, wantTx)
	}
	if b.Height != height || b.Round != round || b.ID != id || b.PreviousID != prev {
		t.Fatalf("block header = {h:%d r:%d id:%s prev:%s}, want {h:%d r:%d id:%s prev:%s}",
			b.Height, b.Round, b.ID, b.PreviousID, height, round, id, prev)
	}
}

// tamperBlock 以调用方身份尽力改写手中的确认块：交换顺序、改标识、追加长度。
func tamperBlock(b *Block) {
	if len(b.TxIDs) >= 2 {
		b.TxIDs[0], b.TxIDs[1] = b.TxIDs[1], b.TxIDs[0]
	}
	b.TxIDs[0] = "tampered-id"
	b.TxIDs = append(b.TxIDs, "appended-id")
}

// Vote 确认时返回的确认块是独立快照：交换、改写、追加只影响调用方手中的结果；
// 随后 BlockAt/LatestBlock 仍是实际胜出候选的原始列表与顺序，
// 高度、轮次、区块标识及前块标识保持原值；交易查询与候选历史同样不受影响。
func TestVoteConfirmedBlockIsSnapshot(t *testing.T) {
	n, _, keys, tx1, tx2 := setupTwoTxNode(t)
	p, _ := n.Propose()
	want := []string{tx2.ID(), tx1.ID()}
	if fmt.Sprint(p.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("setup order = %v, want %v", p.TxIDs, want)
	}

	var confirmed *VoteResult
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, p.BlockID)
		if err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			// 尚未达到确认条件时不返回确认块。
			if res.Confirmed || res.Block != nil {
				t.Fatalf("vote %d must not return a block: %+v", i+1, res)
			}
		} else {
			if !res.Confirmed || res.Block == nil {
				t.Fatalf("third vote should confirm: %+v", res)
			}
			confirmed = res
		}
	}
	assertBlock(t, *confirmed.Block, want, 1, 1, p.BlockID, "")

	// 调用方尽力改写 Vote 返回结果。
	tamperBlock(confirmed.Block)

	got, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, got, want, 1, 1, p.BlockID, "")
	latest, ok := n.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist")
	}
	assertBlock(t, latest, want, 1, 1, p.BlockID, "")

	// 区块标识与交易查询仍指向原来的确认结果。
	for _, tx := range []*Transaction{tx1, tx2} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
			t.Fatalf("tx %s lookup wrong: %+v", tx.ID(), info)
		}
	}

	// 候选历史中的胜出交易列表与确认块一致。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	var winner *CandidateView
	for i := range rc.Candidates {
		if rc.Candidates[i].Result == CandidateWon {
			winner = &rc.Candidates[i]
		}
	}
	if winner == nil || winner.BlockID != p.BlockID || fmt.Sprint(winner.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("winner candidate history wrong: %+v", winner)
	}
}

// 同一高度的多份返回结果互不影响：改一份，其他已取得结果保持原样；
// Vote 确认块与查询取得的区块之间也互不别名。
func TestMultipleBlockSnapshotsIndependent(t *testing.T) {
	n, _, keys, tx1, tx2 := setupTwoTxNode(t)
	p, _ := n.Propose()
	want := []string{tx2.ID(), tx1.ID()}
	confirmByVotes(t, n, keys, p.BlockID)

	b1, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := n.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist")
	}

	// 改写先取得的一份，其余已取得的结果保持原样。
	tamperBlock(&b1)
	assertBlock(t, b2, want, 1, 1, p.BlockID, "")
	assertBlock(t, latest, want, 1, 1, p.BlockID, "")

	// 再改写 LatestBlock 结果，BlockAt 结果与新查询仍保持原样。
	tamperBlock(&latest)
	assertBlock(t, b2, want, 1, 1, p.BlockID, "")
	b3, _ := n.BlockAt(1)
	assertBlock(t, b3, want, 1, 1, p.BlockID, "")
}

// 本地修改不会随正常操作进入持久化历史：把返回列表第一笔改成不存在的标识、
// 追加标识后，合法提交一笔新交易使节点保存状态；停止重开同一状态目录，
// 原确认块仍保留真正确认的交易及其顺序与区块标识，篡改标识不作为交易存在。
func TestBlockSnapshotMutationNeverPersisted(t *testing.T) {
	n, dir, keys, tx1, tx2 := setupTwoTxNode(t)
	p, _ := n.Propose()
	want := []string{tx2.ID(), tx1.ID()}
	res := confirmByVotesResult(t, n, keys, p.BlockID)

	// 同时篡改 Vote 返回块与 BlockAt 返回块。
	tamperBlock(res.Block)
	got, _ := n.BlockAt(1)
	got.TxIDs[0] = "nonexistent-id"
	got.TxIDs = append(got.TxIDs, "phantom-id")

	// 合法提交一笔新交易，触发正常保存。
	tx3 := NewTransaction(keys[0].priv, 2, []byte("c"), 1, 100)
	if _, err := n.Submit(tx3); err != nil {
		t.Fatalf("legitimate submit after tamper failed: %v", err)
	}

	// 停止后重新打开同一状态目录，历史保持真正确认的结果。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Height() != 1 || reopened.CurrentRound() != 2 {
		t.Fatalf("reopened state wrong: height=%d round=%d", reopened.Height(), reopened.CurrentRound())
	}
	stored, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, stored, want, 1, 1, p.BlockID, "")
	storedLatest, ok := reopened.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist after reopen")
	}
	assertBlock(t, storedLatest, want, 1, 1, p.BlockID, "")

	// 原交易查询仍显示原确认高度与区块标识。
	for _, tx := range []*Transaction{tx1, tx2} {
		info, err := reopened.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
			t.Fatalf("tx %s lookup after reopen wrong: %+v", tx.ID(), info)
		}
	}
	// 篡改出来的标识没有进入历史。
	if _, err := reopened.Tx("nonexistent-id"); reason(err) != ReasonUnknownTx {
		t.Fatalf("tampered id persisted as transaction: %v", err)
	}
	// 新交易正常待处理，账户确认序号仍为真实历史推进的结果。
	info, err := reopened.Tx(tx3.ID())
	if err != nil || info.Status != StatusQueued {
		t.Fatalf("new tx after reopen wrong: %+v %v", info, err)
	}
	// 候选历史中的胜出列表与确认块一致。
	rc, err := reopened.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rc.Candidates {
		if c.Result == CandidateWon && (c.BlockID != p.BlockID || fmt.Sprint(c.TxIDs) != fmt.Sprint(want)) {
			t.Fatalf("winner history inconsistent after reopen: %+v", c)
		}
	}
}

// 胜出者是竞争候选时，确认块返回结果同样遵守快照规则，且篡改不会落盘。
func TestCompetingWinnerBlockIsSnapshotAndNotPersisted(t *testing.T) {
	n, dir, keys, tx1, tx2 := setupTwoTxNode(t)
	local, _ := n.Propose() // 本地提议顺序 [tx2, tx1]。
	// 竞争候选以相反顺序登记并胜出。
	altWant := []string{tx1.ID(), tx2.ID()}
	alt, err := n.RegisterCandidate(1, altWant)
	if err != nil {
		t.Fatal(err)
	}
	res := confirmByVotesResult(t, n, keys, alt.BlockID)
	assertBlock(t, *res.Block, altWant, 1, 1, alt.BlockID, "")

	// 篡改 Vote 返回块：顺序与列表长度都被改变。
	tamperBlock(res.Block)
	got, _ := n.BlockAt(1)
	assertBlock(t, got, altWant, 1, 1, alt.BlockID, "")

	// 候选历史：竞争候选胜出且列表与确认块一致，本地提议落选。
	rc, _ := n.Candidates(1)
	for _, c := range rc.Candidates {
		switch c.BlockID {
		case alt.BlockID:
			if c.Result != CandidateWon || fmt.Sprint(c.TxIDs) != fmt.Sprint(altWant) {
				t.Fatalf("competing winner history wrong: %+v", c)
			}
		case local.BlockID:
			if c.Result != CandidateLost {
				t.Fatalf("local candidate should be lost: %+v", c)
			}
		}
	}

	// 触发一次正常保存后重开，竞争候选的原始胜出结果保持不变。
	tx3 := NewTransaction(keys[0].priv, 2, []byte("c"), 1, 100)
	if _, err := n.Submit(tx3); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := reopened.BlockAt(1)
	assertBlock(t, stored, altWant, 1, 1, alt.BlockID, "")
	for _, tx := range []*Transaction{tx1, tx2} {
		info, err := reopened.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != alt.BlockID {
			t.Fatalf("competing-winner tx %s wrong after reopen: %+v", tx.ID(), info)
		}
	}
}

// 空确认块仍返回零笔交易：给返回列表追加标识不能使节点历史多出交易，
// 也不会在后续保存后落盘。
func TestEmptyBlockSnapshotStaysEmpty(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty {
		t.Fatal("setup: want empty proposal")
	}
	res := confirmByVotesResult(t, n, keys, p.BlockID)
	if len(res.Block.TxIDs) != 0 {
		t.Fatalf("empty block must return zero txs, got %v", res.Block.TxIDs)
	}

	// 调用方向 Vote 返回块追加标识。
	res.Block.TxIDs = append(res.Block.TxIDs, "phantom-from-vote")
	if got, _ := n.BlockAt(1); len(got.TxIDs) != 0 {
		t.Fatalf("vote append leaked into history: %v", got.TxIDs)
	}
	// 调用方向 LatestBlock 返回块追加标识。
	latest, ok := n.LatestBlock()
	if !ok {
		t.Fatal("empty block should exist")
	}
	latest.TxIDs = append(latest.TxIDs, "phantom-from-latest")
	if got, _ := n.BlockAt(1); len(got.TxIDs) != 0 {
		t.Fatalf("latest append leaked into history: %v", got.TxIDs)
	}

	// 进入下一轮并触发正常保存，重开后历史仍是零笔交易的空块。
	if _, err := n.Propose(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Height() != 1 {
		t.Fatalf("height = %d, want 1", reopened.Height())
	}
	stored, err := reopened.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.TxIDs) != 0 || stored.ID != p.BlockID || stored.Height != 1 || stored.Round != 1 {
		t.Fatalf("empty block changed after reopen: %+v", stored)
	}
	storedLatest, ok := reopened.LatestBlock()
	if !ok || len(storedLatest.TxIDs) != 0 || storedLatest.ID != p.BlockID {
		t.Fatalf("latest empty block wrong after reopen: %+v ok=%v", storedLatest, ok)
	}
}

// 边界行为保持兼容：尚无确认块时 LatestBlock 不存在；高度 0 或超过最新高度
// 的 BlockAt 以 unknown-block 拒绝。
func TestBlockQueryBoundariesUnchanged(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	if _, ok := n.LatestBlock(); ok {
		t.Fatal("LatestBlock must report absent before any confirmation")
	}
	if _, err := n.BlockAt(0); reason(err) != ReasonUnknownBlock {
		t.Fatalf("height 0 got %v, want %s", err, ReasonUnknownBlock)
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("height beyond tip got %v, want %s", err, ReasonUnknownBlock)
	}

	p, _ := n.Propose()
	// 未确认前按高度仍查不到块。
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("unconfirmed height got %v, want %s", err, ReasonUnknownBlock)
	}
	confirmByVotes(t, n, keys, p.BlockID)
	if _, err := n.BlockAt(0); reason(err) != ReasonUnknownBlock {
		t.Fatalf("height 0 after confirm got %v, want %s", err, ReasonUnknownBlock)
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("height beyond tip after confirm got %v, want %s", err, ReasonUnknownBlock)
	}
	if latest, ok := n.LatestBlock(); !ok || latest.ID != p.BlockID || latest.Height != 1 {
		t.Fatalf("latest after confirm wrong: %+v ok=%v", latest, ok)
	}
}

// confirmByVotesResult 投票至确认并返回决定性那次的结果（含确认块）。
func confirmByVotesResult(t *testing.T, n *Node, keys []testKey, blockID string) *VoteResult {
	t.Helper()
	round := n.CurrentRound()
	count := len(keys)*2/3 + 1
	var last *VoteResult
	for i := 0; i < count; i++ {
		res, err := n.Vote(keys[i].pub, round, blockID)
		if err != nil {
			t.Fatalf("vote %d: %v", i, err)
		}
		last = res
	}
	if !last.Confirmed || last.Block == nil {
		t.Fatalf("block %s was not confirmed", blockID)
	}
	return last
}
