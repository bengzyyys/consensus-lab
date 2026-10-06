// Command competing-candidates 演示竞争候选如何通过投票胜出本地提议。
//
// 流程：四名验证者、固定种子、单块上限两笔交易；提交费用 20 与 10 的
// 两笔交易，本地提议打包两笔，再登记只含费用 20 那笔的竞争候选；一名
// 验证者投本地提议，其余三名依次投竞争候选，第三票使其达到“严格超过
// 2/3”的门槛而确认。运行：
//
//	go run ./examples/competing-candidates
package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/bengzyyys/consensus-lab/consensus"
)

// keyFromSeed 从固定标签派生确定性的 Ed25519 密钥，保证示例输出可复现。
func keyFromSeed(label string) ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, label)
	return ed25519.NewKeyFromSeed(seed)
}

// check 处理正常调用中不应出现的错误；预期的拒绝（如改投）单独判定。
func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	log.SetFlags(0)

	// 新的状态目录（演示结束即清理）。
	dir, err := os.MkdirTemp("", "consensus-demo-")
	check(err)
	defer os.RemoveAll(dir)

	// 四名验证者与两个交易账户，密钥均由固定标签派生，均为合法 Ed25519 密钥。
	names := []string{"validator0", "validator1", "validator2", "validator3"}
	valPubs := make([][]byte, len(names))
	valName := map[string]string{}
	for i, name := range names {
		priv := keyFromSeed("demo-" + name)
		pub := priv.Public().(ed25519.PublicKey)
		valPubs[i] = pub
		valName[fmt.Sprintf("%x", pub)] = name
	}
	alice := keyFromSeed("demo-account-alice")
	bob := keyFromSeed("demo-account-bob")

	n, err := consensus.New(dir, consensus.Config{
		Seed:           []byte("competing-candidates-demo"),
		Validators:     valPubs,
		MaxTxsPerBlock: 2,
	})
	check(err)

	// 两笔交易分属不同账户，序号均为 1，费用分别为 20 与 10，
	// 到期轮次远大于演示范围，演示期间不会到期。
	txFee20 := consensus.NewTransaction(alice, 1, []byte("alice: fee 20"), 20, 1000)
	txFee10 := consensus.NewTransaction(bob, 1, []byte("bob: fee 10"), 10, 1000)
	res20, err := n.Submit(txFee20)
	check(err)
	res10, err := n.Submit(txFee10)
	check(err)
	fee20ID, fee10ID := res20.TxID, res10.TxID
	feeOf := map[string]uint64{fee20ID: 20, fee10ID: 10}
	fmt.Printf("已提交费用 20 的交易 %s\n", fee20ID)
	fmt.Printf("已提交费用 10 的交易 %s\n", fee10ID)

	// 本地提议：单块上限为 2，两笔都被打包；打包规则先取各账户
	// 下一条可确认交易中费用最高者，故费用 20 的交易排在前面。
	p, err := n.Propose()
	check(err)
	round := p.Round // 保存轮次值：确认之后查询这一轮要用它
	fmt.Printf("\n第 %d 轮本地提议 %s，打包顺序：\n", p.Round, p.BlockID)
	for i, id := range p.TxIDs {
		fmt.Printf("  %d. %s（费用 %d）\n", i+1, id, feeOf[id])
	}

	// 竞争候选：只包含费用 20 那笔交易，与本地提议同轮竞争。
	// 交易列表不同，区块标识也不同。
	reg, err := n.RegisterCandidate(round, []string{fee20ID})
	check(err)
	rivalID := reg.BlockID
	fmt.Printf("\n已登记竞争候选 %s，仅含交易 %s\n", rivalID, fee20ID)

	// validator0 投给本地提议。
	vr, err := n.Vote(valPubs[0], round, p.BlockID)
	check(err)
	fmt.Printf("\nvalidator0 投给本地提议（计入: %v）\n", vr.Counted)

	// validator1、validator2 依次投给竞争候选：两票仍未达到确认门槛。
	for i := 1; i <= 2; i++ {
		vr, err = n.Vote(valPubs[i], round, rivalID)
		check(err)
		fmt.Printf("%s 投给竞争候选（第 %d 票，确认: %v）\n", names[i], i, vr.Confirmed)
	}
	fmt.Println("4 人名单要求票数严格超过 2/3（即 > 2 票），2 票尚不能确认")

	// validator0 尝试改投竞争候选：预期被 already-voted 拒绝，原票保留。
	// 用 errors.As 取出 *RejectError 并核对 Reason，把这一预期拒绝与
	// 其他失败区分开。
	_, err = n.Vote(valPubs[0], round, rivalID)
	var rej *consensus.RejectError
	switch {
	case errors.As(err, &rej) && rej.Reason == consensus.ReasonAlreadyVoted:
		fmt.Printf("validator0 改投被预期拒绝: %v\n", err)
	case err == nil:
		log.Fatal("改投竟被接受，违反一位验证者一轮只能投一个候选的规则")
	default:
		log.Fatalf("改投出现预期外错误: %v", err)
	}

	// validator3 的第三票使竞争候选胜出并立即确认。
	vr, err = n.Vote(valPubs[3], round, rivalID)
	check(err)
	if !vr.Confirmed || vr.Block == nil {
		log.Fatal("第三票应使竞争候选确认")
	}
	fmt.Printf("validator3 投给竞争候选（第 3 票，确认: %v，区块高度 %d）\n", vr.Confirmed, vr.Block.Height)

	// 用保存的轮次值查询刚结束的轮次：竞争候选胜出，本地提议落选，
	// 各自保留的投票者清晰可见。
	rc, err := n.Candidates(round)
	check(err)
	fmt.Printf("\n第 %d 轮候选结果（节点当前已进入第 %d 轮）：\n", round, n.CurrentRound())
	for _, c := range rc.Candidates {
		kind := "竞争候选"
		if c.Local {
			kind = "本地提议"
		}
		voters := make([]string, 0, len(c.Voters))
		for _, v := range c.Voters {
			voters = append(voters, valName[fmt.Sprintf("%x", v)])
		}
		fmt.Printf("  %s %s: %s，投票者 %v\n", kind, c.BlockID, c.Result, voters)
	}

	// 费用 20 的交易随胜出候选确认，其账户已确认序号推进为 1。
	info20, err := n.Tx(fee20ID)
	check(err)
	fmt.Printf("\n费用 20 的交易: %s，确认于高度 %d 的区块 %s\n", info20.Status, info20.BlockHeight, info20.BlockID)
	accAlice := n.Account(txFee20.Sender)
	fmt.Printf("  alice 已确认序号: %d\n", accAlice.ConfirmedSequence)

	// 费用 10 的交易仅被落选的本地提议引用，回到排队等待打包，
	// 其账户已确认序号仍为 0。
	info10, err := n.Tx(fee10ID)
	check(err)
	accBob := n.Account(txFee10.Sender)
	fmt.Printf("费用 10 的交易: %s（落选提议的独有交易回到排队）\n", info10.Status)
	fmt.Printf("  bob 已确认序号: %d，待处理 %d 笔（%s）\n",
		accBob.ConfirmedSequence, len(accBob.Pending), accBob.Pending[0].Note)

	// 本地提议落选不会生成另一条确认历史。
	fmt.Printf("\n确认历史共 %d 个区块：本地提议落选不产生另一条确认记录\n", n.Height())
}
