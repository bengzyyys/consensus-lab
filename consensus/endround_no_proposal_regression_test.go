package consensus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// 本文件钉住“尚无本地提议时主动结束轮次”的回归保障。
//
// 现有行为：当前轮次尚未产生本地提议（也未登记任何候选）时，仍可直接调用
// EndRound。这种结束只推进仿真轮次并处理到期交易，不代表确认了一个空块；
// 节点已有确认历史、账户已确认序号非零时同样适用。
//
// 成功结束后：返回轮次与当前轮次均为原轮次加一，新轮次仍没有本地提议；
// 刚结束的轮次可查——已结束、未确认结束、候选列表为空、详情记录存在
// （HasRecords 为 true，不得被当作旧版本未保存详情的轮次，也不得凭空出现
// 空块候选），全部验证者按公钥次序列在未投票名单中；确认高度、最新确认块、
// 已有区块交易顺序与账户已确认序号保持原样；新轮次尚未生成提议时，候选查询
// 按现有约定返回 unknown-round。
//
// 轮次推进照常处理交易到期：同一账户下一笔待确认交易恰好在新轮次到期时变为
// expired（完整内容仍可查询、移出待处理列表），更高序号的有效交易继续 queued，
// 账户显示 waiting-pack、最早缺口变为刚退出的序号；另一账户未到期的排队交易
// 继续等待打包，不因主动结束进入等待投票。到期依据是进入后的轮次，费用高低
// 不改变失效结果。完全没有待处理交易时也允许直接结束。
//
// 保存失败时：返回错误与轮次 0，当前轮次、候选记录、交易状态与账户缺口全部
// 保持操作前结果，不留刚结束的轮次记录，也不提前移除将要到期的交易；保存恢复
// 正常后再次结束按同一组规则成功。本组测试只补足回归保障，不改变已有本地提议
// 时的结束方式、投票确认方式与公开调用。

// noProposalTxs 是无本地提议结束场景中第 2 轮池内的三笔交易。
type noProposalTxs struct {
	// Expiring 账户0 的下一笔待确认交易（seq2），到期轮次恰好等于结束后的新轮次；
	// 费用刻意给全场景最高，钉住“费用高低不能改变失效结果”。
	Expiring *Transaction
	// Surviving 账户0 的更高序号交易（seq3），到期轮次很远，结束后继续排队。
	Surviving *Transaction
	// Other 账户1 的未到期排队交易，结束后继续等待打包。
	Other *Transaction
}

// buildNoProposalRoundTwo 构造“已有确认历史、第 2 轮尚无本地提议”的场景：
// 第 1 轮确认账户0 seq1（高度 1，账户0 已确认序号为 1），进入第 2 轮后只提交
// 三笔交易、不调用 Propose。返回第 1 轮的确认块与三笔交易。
func buildNoProposalRoundTwo(t *testing.T, n *Node, keys []testKey) (Block, noProposalTxs) {
	t.Helper()
	hist := NewTransaction(keys[0].priv, 1, []byte("round1-confirmed"), 5, 100)
	if _, err := n.Submit(hist); err != nil {
		t.Fatal(err)
	}
	p1, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(p1.TxIDs) != fmt.Sprint([]string{hist.ID()}) {
		t.Fatalf("setup: round 1 proposal = %v, want [%s]", p1.TxIDs, hist.ID())
	}
	// 四验证者门槛为 3 票，第三票立即确认并进入第 2 轮。
	for i := 0; i < 3; i++ {
		res, err := n.Vote(keys[i].pub, 1, p1.BlockID)
		if err != nil || !res.Counted {
			t.Fatalf("round 1 confirming vote %d: %+v %v", i+1, res, err)
		}
		if (i == 2) != res.Confirmed {
			t.Fatalf("round 1 vote %d confirmed=%v, want %v", i+1, res.Confirmed, i == 2)
		}
	}
	if n.CurrentRound() != 2 || n.Height() != 1 {
		t.Fatalf("setup: round=%d height=%d, want 2/1", n.CurrentRound(), n.Height())
	}
	confirmedBlock, err := n.BlockAt(1)
	if err != nil {
		t.Fatal(err)
	}

	txs := noProposalTxs{
		Expiring:  NewTransaction(keys[0].priv, 2, []byte("expiring-next-round"), 50, 3),
		Surviving: NewTransaction(keys[0].priv, 3, []byte("surviving"), 1, 100),
		Other:     NewTransaction(keys[1].priv, 1, []byte("other-account"), 7, 100),
	}
	for _, tx := range []*Transaction{txs.Expiring, txs.Surviving, txs.Other} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatalf("submit %q: %v", tx.Content, err)
		}
	}
	return confirmedBlock, txs
}

// sortedValidatorHex 返回全部验证者公钥十六进制的字典序列表，
// 即 Candidates 未投票名单遵循的现有公钥次序。
func sortedValidatorHex(keys []testKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%x", k.pub))
	}
	sort.Strings(out)
	return out
}

// assertEmptyEndedRound 断言给定轮次是一条“主动结束、无候选”的记录：
// 已结束且未确认、详情记录存在（不是旧版本未保存详情的轮次）、候选列表为空
// （不凭空出现空块候选），全部验证者按公钥次序列在未投票名单中。
func assertEmptyEndedRound(t *testing.T, n *Node, round uint64, keys []testKey) {
	t.Helper()
	rc, err := n.Candidates(round)
	if err != nil {
		t.Fatalf("ended round %d must stay queryable: %v", round, err)
	}
	if !rc.Ended || !rc.UnconfirmedEnd {
		t.Fatalf("round %d must be an unconfirmed ended round: %+v", round, rc)
	}
	if !rc.HasRecords {
		t.Fatalf("round %d has a recorded detail and must not look like a legacy round without records", round)
	}
	if len(rc.Candidates) != 0 {
		t.Fatalf("round %d ended without any proposal and must not gain candidates: %+v", round, rc.Candidates)
	}
	if fmt.Sprint(hexKeys(rc.Unvoted)) != fmt.Sprint(sortedValidatorHex(keys)) {
		t.Fatalf("round %d unvoted validators = %x, want all validators in public-key order", round, rc.Unvoted)
	}
}

// assertNoProposalPending 断言第 2 轮尚无本地提议的完整操作前状态
// （也用于保存失败后的整体回滚校验）：轮次未前进；当前轮次候选查询按现有约定
// 返回 unknown-round（不留刚结束的轮次记录）；三笔交易全部 queued 且仍在账户
// 待处理列表中（将要到期的交易不被提前移除）；确认历史与账户已确认序号原样。
func assertNoProposalPending(t *testing.T, n *Node, keys []testKey, txs noProposalTxs, confirmedBlock Block) {
	t.Helper()
	if n.CurrentRound() != 2 {
		t.Fatalf("current round = %d, want 2", n.CurrentRound())
	}
	assertConfirmedHistory(t, n, confirmedBlock, keys)

	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 must have no local proposal: %+v", p)
	}
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("round 2 without a proposal: %v, want %s", err, ReasonUnknownRound)
	}

	for _, tx := range []*Transaction{txs.Expiring, txs.Surviving, txs.Other} {
		info := n.mustTx(t, tx.ID())
		if info.Status != StatusQueued {
			t.Fatalf("tx %q status = %s, want queued", tx.Content, info.Status)
		}
		if info.BlockHeight != 0 || info.BlockID != "" {
			t.Fatalf("tx %q must not reference a block: height=%d block=%q",
				tx.Content, info.BlockHeight, info.BlockID)
		}
	}
	// 账户0 已确认序号为 1，seq2/seq3 连续排队、无缺口，均等待打包。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{
		txs.Expiring.ID():  "waiting-pack",
		txs.Surviving.ID(): "waiting-pack",
	})
	if a := n.Account(keys[0].pub); a.Gap != 0 {
		t.Fatalf("account 0 gap = %d, want 0", a.Gap)
	}
	// 账户1 已确认序号为 0，唯一交易等待打包。
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{txs.Other.ID(): "waiting-pack"})
}

// assertNoProposalEnded 断言无本地提议的 EndRound 成功进入第 3 轮后的完整状态。
func assertNoProposalEnded(t *testing.T, n *Node, keys []testKey, txs noProposalTxs, confirmedBlock Block) {
	t.Helper()
	if n.CurrentRound() != 3 {
		t.Fatalf("current round = %d, want 3", n.CurrentRound())
	}
	// 主动结束不确认空块：确认高度、最新确认块、已有区块交易顺序与账户已确认序号原样。
	assertConfirmedHistory(t, n, confirmedBlock, keys)

	// 刚结束的第 2 轮：已结束、未确认、记录存在、候选为空、全员未投票。
	assertEmptyEndedRound(t, n, 2, keys)

	// 新轮次仍没有本地提议；尚未生成提议时候选查询按现有约定返回 unknown-round。
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 3 must have no local proposal yet: %+v", p)
	}
	if _, err := n.Candidates(3); reason(err) != ReasonUnknownRound {
		t.Fatalf("round 3 without a proposal: %v, want %s", err, ReasonUnknownRound)
	}

	// 恰好在新轮次到期的交易变为 expired：完整内容保留可查，不带确认块关联；
	// 它的费用是全场景最高的 50，失效结果不随费用改变。
	exp := n.mustTx(t, txs.Expiring.ID())
	if exp.Status != StatusExpired {
		t.Fatalf("expiring tx status = %s, want expired", exp.Status)
	}
	if exp.Tx == nil || exp.Tx.Sequence != 2 || string(exp.Tx.Content) != "expiring-next-round" ||
		exp.Tx.Fee != 50 || exp.Tx.Expiry != 3 {
		t.Fatalf("expired tx must keep its full content: %+v", exp.Tx)
	}
	if exp.BlockHeight != 0 || exp.BlockID != "" {
		t.Fatalf("expired tx must not reference a block: height=%d block=%q", exp.BlockHeight, exp.BlockID)
	}
	// 更高序号的有效交易与另一账户的未到期交易继续 queued。
	if info := n.mustTx(t, txs.Surviving.ID()); info.Status != StatusQueued {
		t.Fatalf("surviving tx status = %s, want queued", info.Status)
	}
	if info := n.mustTx(t, txs.Other.ID()); info.Status != StatusQueued {
		t.Fatalf("other account tx status = %s, want queued", info.Status)
	}

	// 账户0：已确认序号仍为 1；到期交易移出待处理列表，更高序号交易继续
	// waiting-pack，最早缺口变成刚退出的序号 2。
	assertAccountQueue(t, n, keys[0].pub, 1, map[string]string{txs.Surviving.ID(): "waiting-pack"})
	if a := n.Account(keys[0].pub); a.Gap != 2 {
		t.Fatalf("account 0 gap after end = %d, want 2 (the just-expired sequence)", a.Gap)
	}
	// 账户1：未到期交易继续等待打包，不因主动结束进入等待投票。
	assertAccountQueue(t, n, keys[1].pub, 0, map[string]string{txs.Other.ID(): "waiting-pack"})
}

// TestEndRoundWithoutProposal 覆盖主场景：已有确认历史、账户已确认序号非零、
// 当前轮次已提交交易但尚无本地提议时，可直接 EndRound；结束只推进轮次并处理
// 到期交易，不确认空块。结果在内存与重开后一致。
func TestEndRoundWithoutProposal(t *testing.T) {
	keys := genKeys(t, 4)
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = keys[i].pub
	}
	dir := t.TempDir()
	n, err := New(dir, Config{Seed: []byte("test-seed-v1"), Validators: vals, MaxTxsPerBlock: maxTestTxs})
	if err != nil {
		t.Fatal(err)
	}

	confirmedBlock, txs := buildNoProposalRoundTwo(t, n, keys)
	assertNoProposalPending(t, n, keys, txs, confirmedBlock)

	// 无须先生成候选，直接结束第 2 轮：返回轮次与当前轮次均为原轮次加一。
	next, err := n.EndRound()
	if err != nil {
		t.Fatalf("EndRound without a local proposal failed: %v", err)
	}
	if next != 3 {
		t.Fatalf("EndRound returned round = %d, want 3", next)
	}
	assertNoProposalEnded(t, n, keys, txs, confirmedBlock)

	// 结束结果已落盘：重开节点看到完全一致的第 3 轮状态。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProposalEnded(t, reopened, keys, txs, confirmedBlock)
}

// TestEndRoundWithoutProposalEmptyPool 覆盖完全没有待处理交易、也没有任何确认
// 历史的场景：新节点第 1 轮即可直接结束，轮次记录与确认历史遵守相同规则；
// 连续结束多个空轮同样允许。
func TestEndRoundWithoutProposalEmptyPool(t *testing.T) {
	keys := genKeys(t, 4)
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = keys[i].pub
	}
	dir := t.TempDir()
	n, err := New(dir, Config{Seed: []byte("test-seed-v1"), Validators: vals, MaxTxsPerBlock: maxTestTxs})
	if err != nil {
		t.Fatal(err)
	}
	if n.CurrentRound() != 1 {
		t.Fatalf("setup: current round = %d, want 1", n.CurrentRound())
	}

	// 第 1 轮没有任何交易与提议，直接结束。
	next, err := n.EndRound()
	if err != nil {
		t.Fatalf("EndRound on an empty round failed: %v", err)
	}
	if next != 2 || n.CurrentRound() != 2 {
		t.Fatalf("after first end: returned=%d current=%d, want 2/2", next, n.CurrentRound())
	}
	assertEmptyEndedRound(t, n, 1, keys)
	// 空轮结束不确认空块：高度仍为 0，不存在最新确认块与高度 1 的块。
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		t.Fatal("no confirmed block may appear after ending an empty round")
	}
	if _, err := n.BlockAt(1); reason(err) != ReasonUnknownBlock {
		t.Fatalf("block at height 1: %v, want %s", err, ReasonUnknownBlock)
	}
	if p, ok := n.Proposal(); ok {
		t.Fatalf("round 2 must have no local proposal yet: %+v", p)
	}
	if _, err := n.Candidates(2); reason(err) != ReasonUnknownRound {
		t.Fatalf("round 2 without a proposal: %v, want %s", err, ReasonUnknownRound)
	}

	// 第 2 轮同样没有待处理交易，可再次直接结束；两轮记录都保留可查。
	next, err = n.EndRound()
	if err != nil {
		t.Fatalf("second EndRound on an empty round failed: %v", err)
	}
	if next != 3 || n.CurrentRound() != 3 {
		t.Fatalf("after second end: returned=%d current=%d, want 3/3", next, n.CurrentRound())
	}
	assertEmptyEndedRound(t, n, 1, keys)
	assertEmptyEndedRound(t, n, 2, keys)
	if n.Height() != 0 {
		t.Fatalf("confirmed height = %d, want 0", n.Height())
	}

	// 空轮结束记录同样持久化。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CurrentRound() != 3 {
		t.Fatalf("reopened current round = %d, want 3", reopened.CurrentRound())
	}
	assertEmptyEndedRound(t, reopened, 1, keys)
	assertEmptyEndedRound(t, reopened, 2, keys)
	if reopened.Height() != 0 {
		t.Fatalf("reopened confirmed height = %d, want 0", reopened.Height())
	}
}

// TestEndRoundWithoutProposalSaveFailure 覆盖无本地提议的 EndRound 保存失败：
// 返回错误与轮次 0，当前轮次、候选记录、交易状态与账户缺口全部保持操作前
// 结果；不留刚结束的轮次记录，将要到期的交易不被提前移除；磁盘逐字节不变、
// 不残留临时文件，重开看到同样的未结束状态。保存恢复后再次结束按同一组规则
// 成功并持久化。
func TestEndRoundWithoutProposalSaveFailure(t *testing.T) {
	keys := genKeys(t, 4)
	vals := make([][]byte, 4)
	for i := range vals {
		vals[i] = keys[i].pub
	}
	dir := t.TempDir()
	n, err := New(dir, Config{Seed: []byte("test-seed-v1"), Validators: vals, MaxTxsPerBlock: maxTestTxs})
	if err != nil {
		t.Fatal(err)
	}

	confirmedBlock, txs := buildNoProposalRoundTwo(t, n, keys)
	assertNoProposalPending(t, n, keys, txs, confirmedBlock)

	// 快照结束前的磁盘文件：失败的保存既不得改写它，也不得残留临时文件。
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 保存这次结束结果时发生写入错误：返回错误与轮次 0。
	n.injectSaveErr = errors.New("disk full (simulated)")
	next, err := n.EndRound()
	n.injectSaveErr = nil
	if err == nil {
		t.Fatal("EndRound must return an error when saving node state fails")
	}
	if reason(err) != "" {
		t.Fatalf("save failure must surface as an infrastructure error, not a rejection %q: %v", reason(err), err)
	}
	if next != 0 {
		t.Fatalf("EndRound return round = %d, want 0 on save failure", next)
	}

	// 内存：轮次、候选记录、交易状态与账户缺口全部保持操作前结果；
	// 将要到期的交易仍在池中排队，不被提前移除。
	assertNoProposalPending(t, n, keys, txs, confirmedBlock)

	// 磁盘：状态文件逐字节保持操作前内容，目录中仅有 state.json。
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("state file changed despite the failed save")
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range dirEntries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[state.json]" {
		t.Fatalf("failed save must not leave temp files, dir contains %v", names)
	}

	// 重开节点：没有留下刚结束的轮次记录，看到同样的操作前状态。
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProposalPending(t, reopened, keys, txs, confirmedBlock)

	// 保存条件恢复正常后，再次结束按同一组规则成功完成。
	next, err = reopened.EndRound()
	if err != nil {
		t.Fatalf("EndRound after recovery failed: %v", err)
	}
	if next != 3 {
		t.Fatalf("EndRound after recovery = %d, want 3", next)
	}
	assertNoProposalEnded(t, reopened, keys, txs, confirmedBlock)

	// 成功的结束同样持久化：再开一次看到完全一致的第 3 轮状态。
	durable, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertNoProposalEnded(t, durable, keys, txs, confirmedBlock)
}
