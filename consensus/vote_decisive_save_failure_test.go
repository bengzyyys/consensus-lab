package consensus

import (
	"errors"
	"fmt"
	"testing"
)

// voterHexSet 返回候选投票者公钥的十六进制集合，便于与名单逐人比对。
func voterHexSet(c *CandidateView) map[string]bool {
	set := make(map[string]bool, len(c.Voters))
	for _, v := range c.Voters {
		set[fmt.Sprintf("%x", v)] = true
	}
	return set
}

// unvotedHexSet 返回未投票验证者公钥的十六进集合。
func unvotedHexSet(rc *RoundCandidates) map[string]bool {
	set := make(map[string]bool, len(rc.Unvoted))
	for _, v := range rc.Unvoted {
		set[fmt.Sprintf("%x", v)] = true
	}
	return set
}

func findCandidate(t *testing.T, rc *RoundCandidates, blockID string) *CandidateView {
	t.Helper()
	for i := range rc.Candidates {
		if rc.Candidates[i].BlockID == blockID {
			return &rc.Candidates[i]
		}
	}
	t.Fatalf("candidate %s not found in round %d", blockID, rc.Round)
	return nil
}

func pubHex(k testKey) string { return fmt.Sprintf("%x", k.pub) }

// assertPreDecisiveState 校验决定性第三票保存失败后（或重开后）的操作前状态：
// 竞争候选恰好保留两票且仍 pending，第三名验证者仍未投票；本地提议标识与交易
// 顺序不变；两笔交易都在等待投票、均未确认；账户已确认序号与待处理说明不变；
// 轮次、确认高度与最新确认块停留在投票前。
func assertPreDecisiveState(t *testing.T, n *Node, keys []testKey, localID, altID string, tx1, tx2 *Transaction) {
	t.Helper()
	if n.CurrentRound() != 1 {
		t.Fatalf("current round = %d, want 1", n.CurrentRound())
	}
	if n.Height() != 0 {
		t.Fatalf("height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block must exist after failed decisive vote")
	}

	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("round must stay pending: %+v", rc)
	}
	alt := findCandidate(t, rc, altID)
	if alt.Result != CandidatePending {
		t.Fatalf("competing candidate result = %s, want pending", alt.Result)
	}
	if got := voterHexSet(alt); !sameValidators(got, keys[0], keys[1]) {
		t.Fatalf("competing candidate voters = %v, want only first two validators", got)
	}
	local := findCandidate(t, rc, localID)
	if local.Result != CandidatePending || len(local.Voters) != 0 {
		t.Fatalf("local candidate wrong: result=%s voters=%d", local.Result, len(local.Voters))
	}
	// 第三名验证者仍属于未投票者（第四名同样未投）。
	if got := unvotedHexSet(rc); !sameValidators(got, keys[2], keys[3]) {
		t.Fatalf("unvoted validators = %v, want third and fourth validators", got)
	}

	// 本地提议的标识与交易顺序保持原样（按费用打包为 [tx2, tx1]）。
	p, ok := n.Proposal()
	if !ok {
		t.Fatal("local proposal must still be available")
	}
	if p.BlockID != localID || fmt.Sprint(p.TxIDs) != fmt.Sprint([]string{tx2.ID(), tx1.ID()}) {
		t.Fatalf("local proposal changed: id=%s txs=%v", p.BlockID, p.TxIDs)
	}

	// 两笔交易都不能提前确认，也不能回到排队：仍在等待投票。
	for _, tx := range []*Transaction{tx1, tx2} {
		info, err := n.Tx(tx.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusProposed || info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %s changed after failed vote: %+v", tx.ID(), info)
		}
	}

	// 两个账户的已确认序号均未推进，待处理交易仍是等待投票说明。
	acct0 := n.Account(keys[0].pub)
	if acct0.ConfirmedSequence != 0 || len(acct0.Pending) != 1 ||
		acct0.Pending[0].ID != tx1.ID() || acct0.Pending[0].Status != StatusProposed ||
		acct0.Pending[0].Note != "waiting-vote" {
		t.Fatalf("account 0 changed after failed vote: %+v", acct0)
	}
	acct1 := n.Account(keys[1].pub)
	if acct1.ConfirmedSequence != 0 || len(acct1.Pending) != 1 ||
		acct1.Pending[0].ID != tx2.ID() || acct1.Pending[0].Status != StatusProposed ||
		acct1.Pending[0].Note != "waiting-vote" {
		t.Fatalf("account 1 changed after failed vote: %+v", acct1)
	}
}

// sameValidators 判断十六进制公钥集合是否恰好包含给定验证者（不多不少）。
func sameValidators(got map[string]bool, want ...testKey) bool {
	if len(got) != len(want) {
		return false
	}
	for _, k := range want {
		if !got[pubHex(k)] {
			return false
		}
	}
	return true
}

// assertCompetingWonState 校验保存恢复后第三票确认竞争候选的完整结果：
// 竞争候选按自身标识与交易顺序生成高度 1 的确认块并进入下一轮；其中交易
// 关联确认块、所属账户序号推进；本地提议独有交易回到排队且其账户序号不变；
// 旧轮记录竞争候选胜出、本地提议落选；两笔交易进入下一轮均未到期。
func assertCompetingWonState(t *testing.T, n *Node, keys []testKey, localID, altID string, tx1, tx2 *Transaction) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	if n.Height() != 1 {
		t.Fatalf("height = %d, want 1", n.Height())
	}
	wantTx := []string{tx1.ID()}
	b, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}
	assertBlock(t, b, wantTx, 1, 1, altID, "")
	latest, ok := n.LatestBlock()
	if !ok {
		t.Fatal("latest block must exist after confirmation")
	}
	assertBlock(t, latest, wantTx, 1, 1, altID, "")

	// 竞争候选中的交易已确认并关联该块；本地提议独有的交易回到排队而非过期。
	info1, err := n.Tx(tx1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info1.Status != StatusConfirmed || info1.BlockHeight != 1 || info1.BlockID != altID {
		t.Fatalf("winning tx wrong: %+v", info1)
	}
	info2, err := n.Tx(tx2.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info2.Status != StatusQueued || info2.BlockHeight != 0 || info2.BlockID != "" {
		t.Fatalf("losing-only tx must be back queued: %+v", info2)
	}

	// 胜出交易所属账户序号推进；落选独有交易所属账户序号不变、交易等待打包。
	acct0 := n.Account(keys[0].pub)
	if acct0.ConfirmedSequence != 1 || len(acct0.Pending) != 0 {
		t.Fatalf("winning account wrong: %+v", acct0)
	}
	acct1 := n.Account(keys[1].pub)
	if acct1.ConfirmedSequence != 0 || len(acct1.Pending) != 1 ||
		acct1.Pending[0].ID != tx2.ID() || acct1.Pending[0].Status != StatusQueued ||
		acct1.Pending[0].Note != "waiting-pack" {
		t.Fatalf("losing account wrong: %+v", acct1)
	}

	// 旧轮查询：竞争候选胜出（三票），本地提议落选，仅剩第四名验证者未投。
	rc, err := n.Candidates(1)
	if err != nil {
		t.Fatal(err)
	}
	if !rc.Ended || rc.UnconfirmedEnd {
		t.Fatalf("round 1 should be confirmed-ended: %+v", rc)
	}
	alt := findCandidate(t, rc, altID)
	if alt.Result != CandidateWon || fmt.Sprint(alt.TxIDs) != fmt.Sprint(wantTx) {
		t.Fatalf("competing candidate should win with its own order: %+v", alt)
	}
	if got := voterHexSet(alt); !sameValidators(got, keys[0], keys[1], keys[2]) {
		t.Fatalf("winner voters = %v, want first three validators", got)
	}
	local := findCandidate(t, rc, localID)
	if local.Result != CandidateLost {
		t.Fatalf("local candidate result = %s, want lost", local.Result)
	}
	if got := unvotedHexSet(rc); !sameValidators(got, keys[3]) {
		t.Fatalf("unvoted after confirm = %v, want only fourth validator", got)
	}
}

// TestDecisiveVoteSaveFailureIsAtomic 回归保障：决定竞争候选胜负的第三票在
// 保存节点状态失败时，既不能只完成投票/确认/换轮的一部分，也不能被当作已接收。
//
// 四名验证者、两笔不同账户且不会在第二轮到期的交易：本地提议包含两笔，
// 竞争候选只包含其中一笔。两票不足以确认；第三名验证者的决定性投票遇保存失败，
// 调用返回错误且结果为空，内存状态与磁盘状态都停留在投票前。保存恢复后同一
// 验证者对同一候选的投票作为新票被接收并立即确认，确认块、序号推进、落选交易
// 回到排队与旧轮胜负记录全部符合既有约定，重开后保持一致。
func TestDecisiveVoteSaveFailureIsAtomic(t *testing.T) {
	keys := genKeys(t, 4)
	n, dir := newTestNode(t, keys, 10)
	// 两笔不同账户交易，到期轮次足够远：进入下一轮也不会失效。
	tx1 := NewTransaction(keys[0].priv, 1, []byte("a"), 1, 100)
	tx2 := NewTransaction(keys[1].priv, 1, []byte("b"), 2, 100)
	if _, err := n.Submit(tx1); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Submit(tx2); err != nil {
		t.Fatal(err)
	}

	// 本地提议按费用打包两笔，顺序固定为 [tx2, tx1]。
	local, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	localID := local.BlockID
	if fmt.Sprint(local.TxIDs) != fmt.Sprint([]string{tx2.ID(), tx1.ID()}) {
		t.Fatalf("local proposal order = %v, want [tx2 tx1]", local.TxIDs)
	}

	// 竞争候选只包含 tx1 一笔；每个账户序号自 1 起连续，登记合法。
	alt, err := n.RegisterCandidate(1, []string{tx1.ID()})
	if err != nil {
		t.Fatal(err)
	}
	altID := alt.BlockID

	// 前两名验证者投给竞争候选：4 人名单需严格超过 2/3（即至少 3 票），
	// 两票不足以确认，两笔交易仍在等待投票、确认序号均未推进。
	for i := 0; i < 2; i++ {
		res, err := n.Vote(keys[i].pub, 1, altID)
		if err != nil {
			t.Fatalf("vote %d: %v", i+1, err)
		}
		if !res.Counted || res.Confirmed || res.Block != nil {
			t.Fatalf("vote %d must be counted without confirming: %+v", i+1, res)
		}
	}
	if n.CurrentRound() != 1 || n.Height() != 0 {
		t.Fatalf("two votes must not decide the round: round=%d height=%d", n.CurrentRound(), n.Height())
	}

	// 第三名验证者的投票本应使竞争候选胜出，但本次保存失败。
	n.injectSaveErr = errors.New("disk full (simulated)")
	res, err := n.Vote(keys[2].pub, 1, altID)
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("decisive vote must return an error when save fails")
	}
	if res != nil {
		t.Fatalf("vote result must be empty on save failure, got %+v", res)
	}

	// 失败后内存中的当前轮次、确认高度、最新确认块、候选、交易与账户均不变。
	assertPreDecisiveState(t, n, keys, localID, altID, tx1, tx2)

	// 重新打开节点：磁盘上同样只有操作前状态——既无确认块，也无仅落盘的第三票。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertPreDecisiveState(t, reopened, keys, localID, altID, tx1, tx2)

	// 保存恢复正常后，同一名验证者再次投给同一竞争候选：作为新票接收并立即确认。
	res2, err := reopened.Vote(keys[2].pub, 1, altID)
	if err != nil {
		t.Fatalf("revote after recovery: %v", err)
	}
	if !res2.Counted || !res2.Confirmed || res2.Block == nil {
		t.Fatalf("revote must be counted as a new vote and confirm: %+v", res2)
	}
	assertBlock(t, *res2.Block, []string{tx1.ID()}, 1, 1, altID, "")
	assertCompetingWonState(t, reopened, keys, localID, altID, tx1, tx2)

	// 再次重开：确认块、交易状态、账户序号与旧轮胜负记录全部持久保持。
	reopened2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertCompetingWonState(t, reopened2, keys, localID, altID, tx1, tx2)
}
