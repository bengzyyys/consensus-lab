// 示例：同一发送者在相同序号上，用一笔费用更高的有效签名交易替换排队中的旧交易。
//
// 流程：提交序号 1、费用 5 的原交易 -> 演示费用不严格更高的替换被 fee-not-higher
// 拒绝 -> 提交费用 9 的新交易完成替换，查询旧记录（replaced）、新交易（queued）
// 与账户队列（waiting-pack、缺口 0）-> 产生本地提议，交易列表只含新交易，
// 新交易进入等待投票 -> 演示提议冻结后再替换被 tx-in-proposal 拒绝，
// 冻结的提议与原有替换关联均不改变。
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

// expectReject 提交 tx 并断言它恰好被 reason 拒绝。
// 提交成功或返回其他原因都属于演示失败：立即终止，不继续输出任何成功结果。
func expectReject(n *consensus.Node, reason, label string, tx *consensus.Transaction) {
	res, err := n.Submit(tx)
	var rej *consensus.RejectError
	switch {
	case errors.As(err, &rej) && rej.Reason == reason:
		fmt.Printf("%s被拒绝（预期）：%v\n", label, rej)
	case err != nil:
		log.Fatalf("%s出现预期外的错误（应为 %s）：%v", label, reason, err)
	default:
		log.Fatalf("%s被意外接受（应被 %s 拒绝）：%+v", label, reason, res)
	}
}

func main() {
	log.SetFlags(0)

	// 四名验证者与一个发送账户。
	validatorKeys := make([]ed25519.PrivateKey, 4)
	validators := make([][]byte, 4)
	for i := range validatorKeys {
		validatorKeys[i] = demoKey(byte(i + 1))
		validators[i] = append([]byte(nil), validatorKeys[i].Public().(ed25519.PublicKey)...)
	}
	senderKey := demoKey(0x51)
	senderPub := senderKey.Public().(ed25519.PublicKey)

	// 每次运行都在新的临时状态目录建立节点，程序退出时删除；
	// 固定种子与固定演示密钥使交易、区块标识等输出可复现。
	dir, err := os.MkdirTemp("", "consensus-demo-*")
	check(err)
	defer os.RemoveAll(dir)

	n, err := consensus.New(dir, consensus.Config{
		Seed:           []byte("fee-replacement-demo"),
		Validators:     validators,
		MaxTxsPerBlock: 2,
	})
	check(err)
	fmt.Printf("节点已建立：轮次 %d，单块上限 2，验证者 4 名（状态保存在新的临时目录，退出时删除）\n",
		n.CurrentRound())

	// 原交易：同一发送者、序号 1、费用 5、到期轮次 100（演示期间不会到达）。
	txOld := consensus.NewTransaction(senderKey, 1, []byte("sender: fee 5"), 5, 100)
	resOld, err := n.Submit(txOld)
	check(err)
	oldID := resOld.TxID
	fmt.Printf("\n已提交原交易（序号 1，费用 5，到期轮次 100）：%s\n", oldID)

	// 误用一（替换之前）：同发送者同序号、内容不同但费用仍为 5 的另一笔有效签名交易。
	// 加费要求费用“严格更高”，故以 fee-not-higher 拒绝；被拒交易不留记录，原交易继续排队。
	txEqualFee := consensus.NewTransaction(senderKey, 1, []byte("sender: another fee 5 body"), 5, 100)
	fmt.Println("\n—— 替换前的误用：同序号、不同内容、费用仍为 5 ——")
	expectReject(n, consensus.ReasonLowFee, "同序号等费交易", txEqualFee)
	if _, err := n.Tx(txEqualFee.ID()); err != nil {
		var rej *consensus.RejectError
		if !(errors.As(err, &rej) && rej.Reason == consensus.ReasonUnknownTx) {
			log.Fatalf("被拒交易应不留记录，查询实际返回：%v", err)
		}
		fmt.Printf("被拒绝的交易不留记录（按其标识查询返回 %s）\n", consensus.ReasonUnknownTx)
	} else {
		log.Fatal("被 fee-not-higher 拒绝的交易不应留下记录")
	}
	infoOld, err := n.Tx(oldID)
	check(err)
	fmt.Printf("原交易继续排队：状态=%s\n", infoOld.Status)

	// 合法的加费替换：在相同序号 1 上提交另一笔有效签名交易，费用提高到 9，
	// 到期轮次仍为 100。这只是池内同一序号位置的新旧交替，不是确认。
	txNew := consensus.NewTransaction(senderKey, 1, []byte("sender: fee 9 replacement"), 9, 100)
	resNew, err := n.Submit(txNew)
	check(err)
	newID := resNew.TxID
	if resNew.ReplacedID != oldID {
		log.Fatalf("提交结果的替换关联 = %q，应为原交易 %s", resNew.ReplacedID, oldID)
	}
	if resNew.EvictedID != "" {
		log.Fatalf("加费替换不应挤出其他交易，实际挤出 %s", resNew.EvictedID)
	}
	fmt.Println("\n—— 加费替换：在序号 1 上提交费用 9 的新交易 ——")
	fmt.Printf("已提交新交易（序号 1，费用 9，到期轮次 100）：%s\n", newID)
	fmt.Printf("提交结果关联被替换的旧标识：%s\n", resNew.ReplacedID)

	// 替换后的记录与账户查询：
	// 旧交易仍可按原标识查到，状态 replaced，替代关联指向新交易；
	// 新交易 queued；账户已确认序号仍为 0，待处理列表只有新交易，waiting-pack，缺口 0。
	infoOld, err = n.Tx(oldID)
	check(err)
	infoNew, err := n.Tx(newID)
	check(err)
	if infoOld.Status != consensus.StatusReplaced || infoOld.ReplacedBy != newID ||
		infoOld.BlockHeight != 0 || infoOld.BlockID != "" {
		log.Fatalf("旧交易记录异常：%+v", infoOld)
	}
	if infoNew.Status != consensus.StatusQueued || infoNew.ReplacedBy != "" {
		log.Fatalf("新交易记录异常：%+v", infoNew)
	}
	acct := n.Account(senderPub)
	fmt.Println("\n替换后查询：")
	fmt.Printf("  旧交易 %s：状态=%s，替代关联指向新交易 %s\n",
		short(oldID), infoOld.Status, infoOld.ReplacedBy)
	fmt.Printf("  新交易 %s：状态=%s\n", short(newID), infoNew.Status)
	fmt.Printf("  账户：已确认序号=%d，待处理交易=%d 笔，缺口=%d\n",
		acct.ConfirmedSequence, len(acct.Pending), acct.Gap)
	for _, p := range acct.Pending {
		fmt.Printf("    待处理 %s：状态=%s，说明=%s\n", short(p.ID), p.Status, p.Note)
	}
	fmt.Printf("  当前轮次=%d，确认高度=%d：替换本身不会确认交易或推进账户序号\n",
		n.CurrentRound(), n.Height())

	// 产生本地提议：打包只取各账户当前排队的交易，故交易列表只包含新标识。
	proposal, err := n.Propose()
	check(err)
	if len(proposal.TxIDs) != 1 || proposal.TxIDs[0] != newID {
		log.Fatalf("本地提议应只包含新交易，实际：%v", proposal.TxIDs)
	}
	fmt.Printf("\n本地提议（轮次 %d）：区块 %s\n", proposal.Round, proposal.BlockID)
	for i, id := range proposal.TxIDs {
		fmt.Printf("  打包顺序 %d：%s\n", i+1, id)
	}

	// 提议后查询：新交易进入等待投票；旧记录仍是 replaced，替代关联保持原样。
	infoNew, err = n.Tx(newID)
	check(err)
	infoOld, err = n.Tx(oldID)
	check(err)
	acct = n.Account(senderPub)
	fmt.Println("\n提议后查询：")
	fmt.Printf("  新交易 %s：状态=%s\n", short(newID), infoNew.Status)
	for _, p := range acct.Pending {
		fmt.Printf("  账户待处理 %s：状态=%s，说明=%s\n", short(p.ID), p.Status, p.Note)
	}
	fmt.Printf("  旧交易 %s：状态=%s，替代关联仍指向 %s（保持原样）\n",
		short(oldID), infoOld.Status, short(infoOld.ReplacedBy))
	fmt.Printf("  账户已确认序号=%d：进入提议只是等待投票，同样没有确认交易\n",
		acct.ConfirmedSequence)

	// 误用二（新交易进入提议之后）：再提交同序号、费用更高（12）的交易。
	// 交易已被未决候选引用，替换被 tx-in-proposal 拒绝，冻结的提议与原有
	// 替代关系都不改变。
	txHigher := consensus.NewTransaction(senderKey, 1, []byte("sender: fee 12 after proposal"), 12, 100)
	fmt.Println("\n—— 提议后的误用：对已进入候选的同序号交易再加费 ——")
	expectReject(n, consensus.ReasonProposalLocked, "同序号更高费交易", txHigher)

	frozen, ok := n.Proposal()
	if !ok {
		log.Fatal("拒绝后本地提议应仍可查询")
	}
	infoOld, err = n.Tx(oldID)
	check(err)
	infoNew, err = n.Tx(newID)
	check(err)
	acct = n.Account(senderPub)
	fmt.Println("拒绝后冻结检查：")
	fmt.Printf("  本地提议区块=%s（与拒绝前相同=%v），交易列表 %d 笔：\n",
		frozen.BlockID, frozen.BlockID == proposal.BlockID, len(frozen.TxIDs))
	for i, id := range frozen.TxIDs {
		fmt.Printf("    打包顺序 %d：%s（即新交易=%v）\n", i+1, id, id == newID)
	}
	if frozen.BlockID != proposal.BlockID || len(frozen.TxIDs) != 1 || frozen.TxIDs[0] != newID {
		log.Fatal("冻结的本地提议在拒绝后发生了变化")
	}
	fmt.Printf("  旧交易 %s：状态=%s，替代关联仍指向 %s（原有替代关系不变）\n",
		short(oldID), infoOld.Status, short(infoOld.ReplacedBy))
	if infoOld.Status != consensus.StatusReplaced || infoOld.ReplacedBy != newID {
		log.Fatalf("旧交易的替换关联被改动：%+v", infoOld)
	}
	fmt.Printf("  新交易 %s：状态=%s（仍在等待投票，未被费用 12 的交易替换）\n",
		short(newID), infoNew.Status)
	fmt.Printf("  账户已确认序号=%d，确认高度=%d：全程没有任何交易得到确认\n",
		acct.ConfirmedSequence, n.Height())
}
