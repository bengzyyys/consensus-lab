package consensus

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// assertBlock 校验确认块的全部字段与交易顺序。
func assertBlock(t *testing.T, b Block, height, round uint64, id, prev string, txIDs []string) {
	t.Helper()
	if b.Height != height || b.Round != round || b.ID != id || b.PreviousID != prev {
		t.Fatalf("block header = (h=%d r=%d id=%s prev=%s), want (h=%d r=%d id=%s prev=%s)",
			b.Height, b.Round, b.ID, b.PreviousID, height, round, id, prev)
	}
	if fmt.Sprint(b.TxIDs) != fmt.Sprint(txIDs) {
		t.Fatalf("block txs = %v, want %v", b.TxIDs, txIDs)
	}
}

// 确认块的所有公开入口返回的都是独立快照：调用方交换顺序、改写标识、
// 增删列表或改写字段，只影响手中的结果；BlockAt/LatestBlock/投票确认结果、
// 交易查询与候选历史始终保持实际胜出候选的原始内容。多份返回结果互不影响。
func TestConfirmedBlockSnapshotsLocalWinner(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 2, 100)
	n.Submit(a1)
	n.Submit(b1)
	p, _ := n.Propose() // 打包顺序：b1(2) 先、a1(1) 后。
	want := []string{b1.ID(), a1.ID()}

	// 投票确认时取得的区块也是快照。
	var voteBlock *Block
	round := n.CurrentRound()
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, round, p.BlockID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if !res.Confirmed || res.Block == nil {
				t.Fatalf("third vote should confirm: %+v", res)
			}
			voteBlock = res.Block
		}
	}
	assertBlock(t, *voteBlock, 1, 1, p.BlockID, "", want)

	// 取得同一高度的多份结果与最新块，之后分别以不同方式篡改，彼此不得影响。
	q1, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	latest1, ok := n.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist")
	}

	voteBlock.TxIDs[0] = "fake-from-vote"
	voteBlock.TxIDs = append(voteBlock.TxIDs, "extra-from-vote")
	voteBlock.Height, voteBlock.Round = 99, 99
	voteBlock.ID, voteBlock.PreviousID = "vid", "vprev"

	q1.TxIDs[0], q1.TxIDs[1] = q1.TxIDs[1], q1.TxIDs[0] // 交换两笔交易
	q1.Height, q1.Round = 7, 7
	q1.ID, q1.PreviousID = "q1id", "q1prev"

	q2.TxIDs[0] = "fake-from-q2" // 把标识改成其他字符串
	q2.TxIDs = q2.TxIDs[:1]      // 调整返回列表长度
	latest1.TxIDs = append(latest1.TxIDs, "fake-from-latest")

	// 再次查询仍得到原始确认结果。
	assertBlock(t, mustBlockAt(t, n, 1), 1, 1, p.BlockID, "", want)
	latest2, ok := n.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist")
	}
	assertBlock(t, latest2, 1, 1, p.BlockID, "", want)

	// 已取得的其他结果也保持原样（同高度多份返回互不影响）。
	assertBlock(t, q1, 7, 7, "q1id", "q1prev", []string{a1.ID(), b1.ID()})
	if len(q2.TxIDs) != 1 || q2.TxIDs[0] != "fake-from-q2" {
		t.Fatalf("q2 mutation lost: %v", q2.TxIDs)
	}

	// 区块标识与交易查询仍指向原确认结果，被篡改引入的标识不存在。
	info, err := n.Tx(a1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
		t.Fatalf("a1 lookup wrong after mutation: %+v", info)
	}
	info, _ = n.Tx(b1.ID())
	if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
		t.Fatalf("b1 lookup wrong after mutation: %+v", info)
	}
	if _, err := n.Tx("fake-from-vote"); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake id lookup got %v, want %s", err, ReasonUnknownTx)
	}

	// 候选历史中的胜出交易列表与确认块一致。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	var won *CandidateView
	for i := range rc.Candidates {
		if rc.Candidates[i].Result == CandidateWon {
			c := rc.Candidates[i]
			won = &c
		}
	}
	if won == nil || won.BlockID != p.BlockID || fmt.Sprint(won.TxIDs) != fmt.Sprint(want) {
		t.Fatalf("winning candidate history wrong: %+v", won)
	}
}

// 竞争候选胜出时，确认块返回结果同样是快照，节点保留竞争候选的原始顺序与标识。
func TestConfirmedBlockSnapshotsCompetingWinner(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 5, 100)
	n.Submit(a1)
	n.Submit(b1)
	local, _ := n.Propose() // 本地顺序 b1、a1。
	list := []string{a1.ID(), b1.ID()}
	alt, err := n.RegisterCandidate(1, list) // 竞争候选相反顺序
	if err != nil {
		t.Fatal(err)
	}

	var voteBlock *Block
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, alt.BlockID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			voteBlock = res.Block
		}
	}
	if voteBlock == nil {
		t.Fatal("competing candidate should confirm")
	}
	assertBlock(t, *voteBlock, 1, 1, alt.BlockID, "", list)

	// 篡改投票结果与查询结果。
	voteBlock.TxIDs[0] = "fake-vote"
	blk := mustBlockAt(t, n, 1)
	blk.TxIDs[0], blk.TxIDs[1] = blk.TxIDs[1], blk.TxIDs[0]
	blk.TxIDs = append(blk.TxIDs, "extra")
	latest, _ := n.LatestBlock()
	latest.ID = "fake-latest-id"

	// 节点内部仍是竞争候选胜出的原始内容。
	assertBlock(t, mustBlockAt(t, n, 1), 1, 1, alt.BlockID, "", list)
	latest2, ok := n.LatestBlock()
	if !ok || latest2.ID != alt.BlockID {
		t.Fatalf("latest block wrong after mutation: %+v ok=%v", latest2, ok)
	}
	if latest2.PreviousID != "" {
		t.Fatalf("genesis prev = %q, want empty", latest2.PreviousID)
	}
	for _, id := range list {
		info, _ := n.Tx(id)
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != alt.BlockID {
			t.Fatalf("winner tx %s wrong: %+v", id, info)
		}
	}
	rc, _ := n.Candidates(1)
	for _, c := range rc.Candidates {
		want := CandidateLost
		var wantTx []string
		if c.BlockID == alt.BlockID {
			want = CandidateWon
			wantTx = list
		}
		if c.Result != want {
			t.Fatalf("candidate %s result = %s, want %s", c.BlockID, c.Result, want)
		}
		if wantTx != nil && fmt.Sprint(c.TxIDs) != fmt.Sprint(wantTx) {
			t.Fatalf("winning candidate txs = %v, want %v", c.TxIDs, wantTx)
		}
	}
	// 本地候选仍然落选且其顺序未受影响。
	localView := mustCandidate(t, rc, local.BlockID)
	if localView.Result != CandidateLost || fmt.Sprint(localView.TxIDs) != fmt.Sprint([]string{b1.ID(), a1.ID()}) {
		t.Fatalf("local candidate wrong: %+v", localView)
	}
}

// 本地改写返回结果后，再经正常操作保存状态、停止重开，篡改不会进入持久化历史：
// 原确认块保留真正确认的交易与顺序，交易查询仍显示原高度与区块标识，
// 候选历史的胜出列表与确认块一致。投票确认时取得的区块遵守同样要求。
func TestConfirmedBlockMutationsDoNotPersist(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	b1 := NewTransaction(keys[1].priv, 1, []byte("b1"), 2, 100)
	n.Submit(a1)
	n.Submit(b1)
	p, _ := n.Propose()
	confirmByVotes(t, n, keys, p.BlockID)
	want1 := []string{b1.ID(), a1.ID()}

	// 调用方把返回列表第一笔改成不存在的标识，并追加一个标识。
	blk := mustBlockAt(t, n, 1)
	blk.TxIDs[0] = "nonexistent-id"
	blk.TxIDs = append(blk.TxIDs, "extra-id")
	latest, _ := n.LatestBlock()
	latest.TxIDs[0] = "nonexistent-id-latest"

	// 合法提交一笔新交易，使节点正常保存状态。
	a2 := NewTransaction(keys[0].priv, 2, []byte("a2"), 1, 100)
	if _, err := n.Submit(a2); err != nil {
		t.Fatal(err)
	}

	// 落盘文件不得包含调用方塞入的任何伪造标识。
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, fake := range []string{"nonexistent-id", "nonexistent-id-latest", "extra-id"} {
		if bytes.Contains(raw, []byte(fake)) {
			t.Fatalf("persisted state contains caller-injected id %q", fake)
		}
	}

	// 停止后重新打开同一状态目录：确认块、交易查询、候选历史全部保持原样。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, mustBlockAt(t, reopened, 1), 1, 1, p.BlockID, "", want1)
	latest2, ok := reopened.LatestBlock()
	if !ok {
		t.Fatal("latest block should exist after reopen")
	}
	assertBlock(t, latest2, 1, 1, p.BlockID, "", want1)
	for _, id := range want1 {
		info, _ := reopened.Tx(id)
		if info.Status != StatusConfirmed || info.BlockHeight != 1 || info.BlockID != p.BlockID {
			t.Fatalf("tx %s after reopen wrong: %+v", id, info)
		}
	}
	if _, err := reopened.Tx("nonexistent-id"); reason(err) != ReasonUnknownTx {
		t.Fatalf("fake id after reopen got %v, want %s", err, ReasonUnknownTx)
	}
	rc, _ := reopened.Candidates(1)
	wonView := mustWinner(t, rc)
	if wonView.BlockID != p.BlockID || fmt.Sprint(wonView.TxIDs) != fmt.Sprint(want1) {
		t.Fatalf("winner history after reopen wrong: %+v", wonView)
	}
	// 新交易仍在池中等待，状态保存未受影响。
	if acct := reopened.Account(keys[0].pub); acct.ConfirmedSequence != 1 || len(acct.Pending) != 1 || acct.Pending[0].ID != a2.ID() {
		t.Fatalf("new tx state after reopen wrong: %+v", acct)
	}

	// 第二轮：投票确认时取得的区块被篡改，再保存、重开后仍是真实结果。
	p2, err := reopened.Propose()
	if err != nil {
		t.Fatal(err)
	}
	var res *VoteResult
	for i := 0; i < 3; i++ {
		res, err = reopened.Vote(keys[i].pub, 2, p2.BlockID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !res.Confirmed || res.Block == nil {
		t.Fatalf("round 2 should confirm: %+v", res)
	}
	want2 := []string{a2.ID()}
	assertBlock(t, *res.Block, 2, 2, p2.BlockID, p.BlockID, want2)
	res.Block.TxIDs[0] = "nonexistent-round2"
	res.Block.TxIDs = append(res.Block.TxIDs, "extra-round2")

	// 再提交一笔交易触发保存。
	b2 := NewTransaction(keys[1].priv, 2, []byte("b2"), 1, 100)
	if _, err := reopened.Submit(b2); err != nil {
		t.Fatal(err)
	}

	reopened2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened2.Height() != 2 {
		t.Fatalf("height after reopen = %d, want 2", reopened2.Height())
	}
	assertBlock(t, mustBlockAt(t, reopened2, 2), 2, 2, p2.BlockID, p.BlockID, want2)
	info, _ := reopened2.Tx(a2.ID())
	if info.Status != StatusConfirmed || info.BlockHeight != 2 || info.BlockID != p2.BlockID {
		t.Fatalf("a2 after reopen wrong: %+v", info)
	}
	rc2, _ := reopened2.Candidates(2)
	won2 := mustWinner(t, rc2)
	if won2.BlockID != p2.BlockID || fmt.Sprint(won2.TxIDs) != fmt.Sprint(want2) {
		t.Fatalf("round 2 winner history wrong: %+v", won2)
	}
	if _, err := reopened2.Tx("nonexistent-round2"); reason(err) != ReasonUnknownTx {
		t.Fatalf("round 2 fake id got %v, want %s", err, ReasonUnknownTx)
	}
}

// 空确认块返回零笔交易；给返回列表追加标识不能使节点历史多出交易。
func TestEmptyBlockSnapshotIsolation(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty {
		t.Fatal("proposal should be empty")
	}
	var res *VoteResult
	for i := 0; i < 3; i++ {
		res, err = n.Vote(keys[i].pub, 1, p.BlockID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !res.Confirmed || res.Block == nil {
		t.Fatalf("empty block should confirm: %+v", res)
	}
	if len(res.Block.TxIDs) != 0 {
		t.Fatalf("empty confirmed block has %d txs", len(res.Block.TxIDs))
	}
	res.Block.TxIDs = append(res.Block.TxIDs, "injected-into-empty-block")

	blk := mustBlockAt(t, n, 1)
	blk.TxIDs = append(blk.TxIDs, "injected-via-query")
	latest, _ := n.LatestBlock()
	latest.TxIDs = append(latest.TxIDs, "injected-via-latest")

	if b := mustBlockAt(t, n, 1); len(b.TxIDs) != 0 || b.ID != p.BlockID || b.Height != 1 || b.Round != 1 {
		t.Fatalf("empty block history polluted: %+v", b)
	}
	if l, ok := n.LatestBlock(); !ok || len(l.TxIDs) != 0 || l.ID != p.BlockID {
		t.Fatalf("latest empty block polluted: %+v ok=%v", l, ok)
	}
	if n.Height() != 1 {
		t.Fatalf("height = %d, want 1", n.Height())
	}
	rc, _ := n.Candidates(1)
	won := mustWinner(t, rc)
	if len(won.TxIDs) != 0 || won.BlockID != p.BlockID {
		t.Fatalf("empty winner history polluted: %+v", won)
	}
}

// 边界行为保持兼容：尚无确认块时 LatestBlock 不存在；高度 0 或超过最新高度
// 以 unknown-block 拒绝；Vote 未达确认条件时不返回确认块。
func TestBlockSnapshotEdgeCasesUnchanged(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 10)

	if _, ok := n.LatestBlock(); ok {
		t.Fatal("LatestBlock must report absent before any confirmation")
	}
	if _, err := n.BlockAt(0); reason(err) != ReasonUnknownBlock {
		t.Fatalf("BlockAt(0) got %v, want %s", err, ReasonUnknownBlock)
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("BlockAt(1) on empty chain got %v, want %s", err, ReasonUnknownBlock)
	}

	a1 := NewTransaction(keys[0].priv, 1, []byte("a1"), 1, 100)
	n.Submit(a1)
	p, _ := n.Propose()
	round := n.CurrentRound()

	// 前两票均未达 >2/3 阈值：不返回确认块。
	for i := 0; i < 2; i++ {
		res, err := n.Vote(keys[i].pub, round, p.BlockID)
		if err != nil {
			t.Fatal(err)
		}
		if res.Confirmed || res.Block != nil {
			t.Fatalf("vote %d must not confirm: %+v", i+1, res)
		}
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("LatestBlock must still be absent before threshold")
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("BlockAt(1) before confirm got %v, want %s", err, ReasonUnknownBlock)
	}

	// 第三票确认后，超过最新高度仍拒绝。
	res, err := n.Vote(keys[2].pub, round, p.BlockID)
	if err != nil || !res.Confirmed || res.Block == nil {
		t.Fatalf("third vote should confirm: %+v %v", res, err)
	}
	if _, err := n.BlockAt(2); reason(err) != ReasonUnknownBlock {
		t.Fatalf("BlockAt(2) got %v, want %s", err, ReasonUnknownBlock)
	}
}

func mustBlockAt(t *testing.T, n *Node, height uint64) Block {
	t.Helper()
	b, err := n.BlockAt(height)
	if err != nil {
		t.Fatalf("BlockAt(%d): %v", height, err)
	}
	return b
}

func mustCandidate(t *testing.T, rc *RoundCandidates, blockID string) CandidateView {
	t.Helper()
	for _, c := range rc.Candidates {
		if c.BlockID == blockID {
			return c
		}
	}
	t.Fatalf("candidate %s not found", blockID)
	return CandidateView{}
}

func mustWinner(t *testing.T, rc *RoundCandidates) CandidateView {
	t.Helper()
	for _, c := range rc.Candidates {
		if c.Result == CandidateWon {
			return c
		}
	}
	t.Fatal("no winning candidate found")
	return CandidateView{}
}
