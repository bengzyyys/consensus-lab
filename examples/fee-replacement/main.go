// 示例：同一发送者在相同序号上，用一笔费用更高、签名有效的新交易替换
// 一笔尚未进入候选的排队交易。
//
// 流程：建立节点 -> 提交序号 1、费用 5 的原交易 -> 尝试提交同序号、
// 内容不同但费用仍为 5 的交易（预期 fee-not-higher）-> 提交同序号、
// 费用提高到 9 的新交易完成替换，查询旧记录、新记录与账户队列 ->
// 产生本地提议，新交易进入等待投票 -> 再次提交更高费用的同序号交易
// （预期 tx-in-proposal），冻结的提议与原有替代关联均不变。
//
// 运行：go run ./examples/fee-replacement
package main

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/bengzyyys/consensus-lab/consensus"
)

// check 处理正常调用的错误：任何非预期失败都直接终止演示。
func check(err error) {
	if err != nil {
		log.Fatalf("unexpected error: %v", err)
	}
}

// demoKey 由固定字节派生演示密钥，使示例输出可复现；
// 真实应用应使用 crypto/rand 随机生成并妥善保管私钥。
func demoKey(b byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
}

// short 截断长标识便于对照阅读；标识本身均来自实际返回值。
func short(id string) string {
	if len(id) > 16 {
		return id[:16] + "…"
	}
	return id
}

// rejectIs 判断 err 是否为指定原因的 *consensus.RejectError。
func rejectIs(err error, reason string) bool {
	var rej *consensus.RejectError
	return errors.As(err, &rej) && rej.Reason == reason
}

// expectReject 核对一次提交是否恰好以预期原因被拒绝：
// 原因不符的其他错误、或操作竟然成功，都视为演示失败并立即终止，
// 绝不把失败当成功继续输出后续结果。
func expectReject(err error, result *consensus.SubmitResult, wantReason, context string) {
	var rej *consensus.RejectError
	switch {
	case errors.As(err, &rej) && rej.Reason == wantReason:
		fmt.Printf("%s被拒绝（预期 %s）：%v\n", context, wantReason, rej)
	case err != nil:
		log.Fatalf("%s应被 %s 拒绝，却出现其他失败：%v", context, wantReason, err)
	default:
		log.Fatalf("%s不应被接受，却返回了成功结果：%+v", context, result)
	}
}

func main() {
	log.SetFlags(0)

	// 四名验证者与一个发送账户（Alice）。
	validatorKeys := make([]ed25519.PrivateKey, 4)
	validators := make([][]byte, 4)
	for i := range validatorKeys {
		validatorKeys[i] = demoKey(byte(i + 1))
		validators[i] = append([]byte(nil), validatorKeys[i].Public().(ed25519.PublicKey)...)
	}
	aliceKey := demoKey(0xa1)
	alicePub := aliceKey.Public().(ed25519.PublicKey)

	// 每次运行都使用新的临时状态目录，退出时清理。
	dir, err := os.MkdirTemp("", "consensus-fee-replace-*")
	check(err)
	defer os.RemoveAll(dir)

	// 固定种子与固定配置：输出可复现。单块上限 2，但本示例池中始终只有
	// 一笔占位交易，因此提议里只会有一笔。
	n, err := consensus.New(dir, consensus.Config{
		Seed:           []byte("fee-replacement-demo"),
		Validators:     validators,
		MaxTxsPerBlock: 2,
	})
	check(err)
	fmt.Printf("节点已建立：轮次 %d，单块上限 2，验证者 4 名（使用新的临时状态目录）\n",
		n.CurrentRound())

	// 原交易：Alice，序号 1，费用 5，到期轮次 100（演示期间不会到达）。
	txLow := consensus.NewTransaction(aliceKey, 1, []byte("alice: seq 1, fee 5"), 5, 100)
	subLow, err := n.Submit(txLow)
	check(err)
	oldID := subLow.TxID
	fmt.Printf("\n已提交原交易：序号 1，费用 5，到期轮次 100\n  标识 %s\n", oldID)

	// 误用一（替换前）：同发送者同序号、内容不同但费用仍为 5 的另一笔有效交易。
	// 费用必须严格更高，故得到 fee-not-higher；被拒交易不留记录，原交易继续排队。
	fmt.Println("\n[误用一] 提交同发送者同序号、内容不同但费用仍为 5 的另一笔交易")
	sameFee := consensus.NewTransaction(aliceKey, 1, []byte("alice: seq 1, equal fee 5"), 5, 100)
	rejected, err := n.Submit(sameFee)
	expectReject(err, rejected, consensus.ReasonLowFee, "费用未严格更高的替换尝试")
	if _, err := n.Tx(sameFee.ID()); !rejectIs(err, consensus.ReasonUnknownTx) {
		log.Fatalf("被 fee-not-higher 拒绝的交易不应留下记录，查询结果却为：%v", err)
	}
	oldInfo, err := n.Tx(oldID)
	check(err)
	if oldInfo.Status != consensus.StatusQueued {
		log.Fatalf("原交易应继续排队，实际状态=%s", oldInfo.Status)
	}
	fmt.Printf("原交易不受影响：状态=%s，继续等待打包\n", oldInfo.Status)

	// 正式加费替换：在相同序号上提交另一笔有效签名的交易，费用提高到 9。
	// 注意这是“新交易”，不是修改旧交易：新内容产生新签名与新标识。
	fmt.Println("\n[加费替换] 提交同发送者、序号仍为 1、费用提高到 9 的新交易（到期轮次仍为 100）")
	txHigh := consensus.NewTransaction(aliceKey, 1, []byte("alice: seq 1, fee 9"), 9, 100)
	subHigh, err := n.Submit(txHigh)
	check(err)
	newID := subHigh.TxID
	if newID == oldID {
		log.Fatal("内容与费用不同，新交易标识必须不同于旧交易")
	}
	if subHigh.ReplacedID != oldID || subHigh.EvictedID != "" {
		log.Fatalf("提交结果=%+v，要求被替换标识为 %s、无挤出交易", subHigh, oldID)
	}
	fmt.Printf("提交成功：新交易标识 %s\n", newID)
	fmt.Printf("提交结果关联的被替换旧标识：%s\n", subHigh.ReplacedID)

	// 旧交易仍可按原标识查询：状态 replaced，替代关联指向新交易，
	// 不带任何确认块信息。
	oldInfo, err = n.Tx(oldID)
	check(err)
	if oldInfo.Status != consensus.StatusReplaced || oldInfo.ReplacedBy != newID ||
		oldInfo.BlockHeight != 0 || oldInfo.BlockID != "" {
		log.Fatalf("旧交易记录异常：%+v", oldInfo)
	}
	fmt.Printf("旧交易查询：状态=%s，替代关联=%s，所在块高度=%d\n",
		oldInfo.Status, oldInfo.ReplacedBy, oldInfo.BlockHeight)

	// 新交易正常排队，没有任何关联。
	newInfo, err := n.Tx(newID)
	check(err)
	if newInfo.Status != consensus.StatusQueued || newInfo.ReplacedBy != "" {
		log.Fatalf("新交易记录异常：%+v", newInfo)
	}
	fmt.Printf("新交易查询：状态=%s，无替代关联\n", newInfo.Status)

	// 账户队列：已确认序号仍为 0；待处理列表只有新交易，等待打包；序号无缺口。
	acc := n.Account(alicePub)
	if acc.ConfirmedSequence != 0 || acc.Gap != 0 || len(acc.Pending) != 1 {
		log.Fatalf("替换后账户状态异常：%+v", acc)
	}
	p := acc.Pending[0]
	if p.ID != newID || p.Status != consensus.StatusQueued || p.Note != "waiting-pack" {
		log.Fatalf("替换后待处理条目异常：%+v", p)
	}
	fmt.Printf("账户查询：已确认序号=%d，缺口=%d，待处理交易=%d 笔\n",
		acc.ConfirmedSequence, acc.Gap, len(acc.Pending))
	fmt.Printf("  待处理 %s：状态=%s，说明=%s\n", short(p.ID), p.Status, p.Note)

	// 替换本身既不确认交易也不推进共识：仍在第 1 轮，没有确认块。
	if n.CurrentRound() != 1 || n.Height() != 0 {
		log.Fatalf("替换不应推进共识，当前轮次=%d、确认高度=%d", n.CurrentRound(), n.Height())
	}
	if _, ok := n.LatestBlock(); ok {
		log.Fatal("替换不应产生确认块")
	}
	fmt.Printf("替换不推进共识：当前轮次=%d，确认高度=%d，尚无确认块\n",
		n.CurrentRound(), n.Height())

	// 产生本地提议：池中只有新交易占位，交易列表只包含新标识。
	fmt.Println("\n[本地提议] 替换完成后产生本轮本地提议")
	proposal, err := n.Propose()
	check(err)
	if len(proposal.TxIDs) != 1 || proposal.TxIDs[0] != newID {
		log.Fatalf("提议交易列表=%v，应只包含新交易 %s", proposal.TxIDs, newID)
	}
	fmt.Printf("本地提议（轮次 %d）：区块 %s，交易 %d 笔\n",
		proposal.Round, proposal.BlockID, len(proposal.TxIDs))
	for i, id := range proposal.TxIDs {
		fmt.Printf("  打包顺序 %d：%s\n", i+1, id)
	}

	// 新交易进入等待投票；账户待处理说明随之改变，但已确认序号仍为 0。
	newInfo, err = n.Tx(newID)
	check(err)
	if newInfo.Status != consensus.StatusProposed {
		log.Fatalf("新交易应随提议进入 %s，实际状态=%s", consensus.StatusProposed, newInfo.Status)
	}
	fmt.Printf("新交易查询：状态=%s（已进入提议，等待投票）\n", newInfo.Status)
	acc = n.Account(alicePub)
	if acc.ConfirmedSequence != 0 || len(acc.Pending) != 1 ||
		acc.Pending[0].ID != newID || acc.Pending[0].Note != "waiting-vote" {
		log.Fatalf("提议后账户状态异常：%+v", acc)
	}
	fmt.Printf("账户查询：已确认序号=%d，待处理交易=%d 笔\n",
		acc.ConfirmedSequence, len(acc.Pending))
	for _, p := range acc.Pending {
		fmt.Printf("  待处理 %s：状态=%s，说明=%s\n", short(p.ID), p.Status, p.Note)
	}

	// 旧记录及其替代关联保持原样，不会因提议而改变。
	oldInfo, err = n.Tx(oldID)
	check(err)
	if oldInfo.Status != consensus.StatusReplaced || oldInfo.ReplacedBy != newID {
		log.Fatalf("提议后旧记录被改动：%+v", oldInfo)
	}
	fmt.Printf("旧交易记录保持原样：状态=%s，替代关联=%s\n",
		oldInfo.Status, oldInfo.ReplacedBy)

	// 误用二（进入提议后）：再提交同序号、费用更高（13）的交易。
	// 交易已被未决候选引用，替换被冻结，得到 tx-in-proposal；
	// 提议、旧记录与替代关联都不变。
	fmt.Println("\n[误用二] 新交易进入提议后，再提交同序号、费用更高（13）的交易")
	txLocked := consensus.NewTransaction(aliceKey, 1, []byte("alice: seq 1, fee 13"), 13, 100)
	lockedRes, err := n.Submit(txLocked)
	expectReject(err, lockedRes, consensus.ReasonProposalLocked, "提议冻结期间的加费替换")
	if _, err := n.Tx(txLocked.ID()); !rejectIs(err, consensus.ReasonUnknownTx) {
		log.Fatalf("被 tx-in-proposal 拒绝的交易不应留下记录，查询结果却为：%v", err)
	}

	// 冻结的提议不变：区块标识与交易列表都和产生时一致。
	frozen, ok := n.Proposal()
	if !ok {
		log.Fatal("本地提议必须仍可查询")
	}
	if frozen.BlockID != proposal.BlockID || len(frozen.TxIDs) != 1 || frozen.TxIDs[0] != newID {
		log.Fatalf("提议被拒绝操作改动：%+v", frozen)
	}
	fmt.Printf("冻结的提议不变：区块 %s，交易列表：\n", frozen.BlockID)
	for i, id := range frozen.TxIDs {
		fmt.Printf("  打包顺序 %d：%s\n", i+1, id)
	}

	// 新交易仍在等待投票；旧记录与替代关联保持原样；账户仍只有新交易待处理。
	newInfo, err = n.Tx(newID)
	check(err)
	oldInfo, err = n.Tx(oldID)
	check(err)
	acc = n.Account(alicePub)
	if newInfo.Status != consensus.StatusProposed ||
		oldInfo.Status != consensus.StatusReplaced || oldInfo.ReplacedBy != newID ||
		acc.ConfirmedSequence != 0 || len(acc.Pending) != 1 || acc.Pending[0].ID != newID {
		log.Fatalf("拒绝后状态发生意外变化：新交易=%s，旧交易=%s/%s，账户=%+v",
			newInfo.Status, oldInfo.Status, oldInfo.ReplacedBy, acc)
	}
	fmt.Printf("新交易仍为 %s；旧交易仍为 %s，替代关联仍指向 %s\n",
		newInfo.Status, oldInfo.Status, oldInfo.ReplacedBy)
	fmt.Printf("演示结束：全程未投票，没有确认块，账户已确认序号始终为 %d\n",
		acc.ConfirmedSequence)
}
