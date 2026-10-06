# 本地共识与交易池仿真

这是一个在本机运行的本地共识与交易池仿真，以 Go 库形式供本地程序调用。

## 使用

```bash
go test ./...
```

## 完整示例：竞争候选通过投票胜出

`examples/competing-candidates` 是一个可直接运行的完整程序，演示竞争候选
功能的正常使用：四名验证者、固定种子、单块上限两笔交易；提交费用 20 与 10
的两笔交易，本地提议打包两笔，再登记只含费用 20 那笔的竞争候选；一名验证者
投本地提议，其余三名依次投竞争候选，第三票使其达到“严格超过 2/3”的门槛而
确认。程序中的标识与状态全部来自库的实际返回结果。

```bash
go run ./examples/competing-candidates
```

完整源码（`examples/competing-candidates/main.go`）：

```go
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
```

### 运行结果与说明

密钥与种子固定，输出完全确定，实际运行结果如下：

```text
已提交费用 20 的交易 55c6b520f30f7f85632e332fbea29bbcdb87c98f36ff70bb98964bd628c88e02
已提交费用 10 的交易 1b5b2aafe75de528fa96aa6c067a8b96318499934f4d7fdf69771bc0ef206b21

第 1 轮本地提议 c644eb86578f1d0075144bfc02e5f99106d8d7a4a63fda5bc3ca3cdea9d2ebc4，打包顺序：
  1. 55c6b520f30f7f85632e332fbea29bbcdb87c98f36ff70bb98964bd628c88e02（费用 20）
  2. 1b5b2aafe75de528fa96aa6c067a8b96318499934f4d7fdf69771bc0ef206b21（费用 10）

已登记竞争候选 1e8f60cccfe8ee3166ba5371c09d39ae230f1a7c66acb82fde6cda5e0df655de，仅含交易 55c6b520f30f7f85632e332fbea29bbcdb87c98f36ff70bb98964bd628c88e02

validator0 投给本地提议（计入: true）
validator1 投给竞争候选（第 1 票，确认: false）
validator2 投给竞争候选（第 2 票，确认: false）
4 人名单要求票数严格超过 2/3（即 > 2 票），2 票尚不能确认
validator0 改投被预期拒绝: already-voted: validator 4fbc76b4… already voted for candidate c644eb86… in round 1 and cannot switch to 1e8f60cc…
validator3 投给竞争候选（第 3 票，确认: true，区块高度 1）

第 1 轮候选结果（节点当前已进入第 2 轮）：
  竞争候选 1e8f60cccfe8ee3166ba5371c09d39ae230f1a7c66acb82fde6cda5e0df655de: won，投票者 [validator3 validator1 validator2]
  本地提议 c644eb86578f1d0075144bfc02e5f99106d8d7a4a63fda5bc3ca3cdea9d2ebc4: lost，投票者 [validator0]

费用 20 的交易: confirmed，确认于高度 1 的区块 1e8f60cccfe8ee3166ba5371c09d39ae230f1a7c66acb82fde6cda5e0df655de
  alice 已确认序号: 1
费用 10 的交易: queued（落选提议的独有交易回到排队）
  bob 已确认序号: 0，待处理 1 笔（waiting-pack）

确认历史共 1 个区块：本地提议落选不产生另一条确认记录
```

（上面改投拒绝一行中的标识做了截断排版，实际输出为完整十六进制串。）

逐段说明：

- **提交与打包**：两笔交易分属 alice、bob 两个账户，序号均为 1，费用 20 与 10。
  本地提议按打包规则把两笔都选入（上限为 2），费用 20 的交易排在第一位——
  每轮从各账户下一条可确认交易中取费用最高者，选入后再考虑该账户后续序号。
- **两个候选的区别**：竞争候选只含费用 20 那一笔交易，交易列表不同，按稳定
  区块标识编码得到的区块标识 `1e8f60cc…` 与本地提议的 `c644eb86…` 不同；
  二者使用相同的轮次、高度与前块标识，仅交易顺序（此处为交易集合）不同。
- **确认门槛**：4 人名单要求票数严格超过 2/3，即至少 3 票。竞争候选拿到
  2 票时 `Vote` 返回 `Confirmed: false`；第三票返回 `Confirmed: true` 并
  立即确认，确认块高度为 1。
- **改投拒绝**：validator0 已投本地提议，在确认前改投竞争候选被
  `already-voted` 拒绝（`*RejectError.Reason == consensus.ReasonAlreadyVoted`），
  原票仍属于本地提议——竞争候选的票数没有因此增加，随后 validator3 的合法
  第三票照常完成确认。示例用 `errors.As` 核对 `Reason`，把这一预期拒绝与
  其他失败区分开。
- **轮次结果**：确认后节点已进入第 2 轮；用之前保存的轮次值调用
  `Candidates(1)` 查询刚结束的第 1 轮，竞争候选为 `won`、本地提议为 `lost`，
  各自保留的投票者清晰可见（投票者按公钥排序返回，故显示顺序与投票先后无关）。
- **交易与账户状态**：费用 20 的交易为 `confirmed`，关联的区块标识正是胜出
  的竞争候选，alice 已确认序号推进为 1；费用 10 的交易仅被落选的本地提议
  引用，回到 `queued`，bob 已确认序号仍为 0，账户查询中该笔显示
  `waiting-pack`（等待打包）。
- **确认历史唯一**：确认历史共 1 个区块——本地提议落选只是候选结果，不会
  生成另一条确认记录。

## 能力概览

- `consensus.New(dir, Config)` / `consensus.Open(dir)`：初始化与恢复；配置包含
  非空种子、固定非空无重复的验证者名单、正整数单块交易上限，以及非负整数交易池
  容量（0 或省略表示不限制，与单块上限相互独立，`Config()` 可查）。轮次从 1 开始，
  账户已确认序号初值为 0。状态保存在指定目录，重启后恢复轮次、交易池、未决候选、
  每人已投的选择、票数、确认历史、容量与淘汰记录；版本 1、2 的旧状态目录仍可打开
  （未保存容量时按不限制处理，确认历史保持原样）。
- `Submit`：交易含发送者公钥、正整数序号、内容、非负费用、到期轮次与 Ed25519 签名。
  拒绝非法签名、已到期、序号不大于已确认序号、重复交易与费用不严格更高的替换，
  原因见 `RejectError.Reason`。同发送者同序号仅保留一笔，旧交易标记为 `replaced`
  并可查到新交易标识；序号有缺口可入池等待。
- 容量限制：计数只含当前排队与等待投票的交易（同一交易被多个候选引用只算一笔；
  已确认、被替换、过期与被挤出的历史不占位置）。现有校验与拒绝原因优先于容量判断；
  合法的加费替换不增加计数，满池也可执行且不淘汰其他交易。全新交易在池满时只能从
  未被任何未决候选引用的排队交易中，挤出费用最低（同费取标识字典序最大）的一笔，
  且仅当新交易费用更高、或费用相同且标识更小时成功，提交结果同时给出被挤出标识；
  否则以 `pool-full` 拒绝，池状态不变且不留历史，腾出位置后可重新提交。被挤出的
  交易保留完整内容，查询显示 `dropped`、原因 `pool-capacity` 与发生轮次，不带确认块
  或替换关联，也不能再登记为候选；淘汰不推进已确认序号，留下的序号缺口照常由账户
  查询指出。接收新交易与淘汰旧交易一起落盘，保存失败时两者状态都保持操作前结果。
- `Propose`：每轮唯一本地提议，重复请求返回同一结果；提议生成后冻结。打包时从各账户
  下一条可确认交易中取费用最高者，费用相同按交易标识字典序，选入后再考虑该账户
  后续序号，直到达到上限；无可选交易产生空块。登记其他候选不改变本地提议。
- `RegisterCandidate`：本地提议已产生且未确认时，可按交易标识顺序登记同轮竞争候选，
  与本地提议一起参与投票。候选使用当前轮次、下一高度并接在最新确认块之后，只能包含
  池内排队或已提议的交易，不超过单块上限，允许空块；列表不重复，每个账户的序号自
  已确认序号加一起连续递增，账户间顺序自定。相同列表重复登记返回同一候选并保留票数。
  未产生本地提议、轮次错误、引用未知或已退出池的交易、超限或序号不连续时拒绝，状态不变。
- `Vote`：名单内验证者对当前轮次任一已登记候选的区块标识投票，票数严格超过名单 2/3
  立即确认该候选，记录连续高度、前块标识与交易顺序，推进序号、移除已确认交易，
  其他候选落选、独有交易回到排队，并进入下一轮。每位验证者一轮只能投一个候选：
  重复投同一候选不增票，改投其他候选明确拒绝且原票保留；名单外身份、错误轮次、
  未知区块标识均被拒绝。
- `EndRound`：主动结束未确认轮次，所有候选落选且不留确认块，候选交易回到排队状态，
  进入新轮次时到期交易失效，旧投票不影响新轮次。
- 查询：`CurrentRound`/`CurrentProposer`、`Proposal`（始终返回本地提议）、
  `Candidates(round)`（按区块标识排序列出候选的交易顺序、投票者与
  pending/won/lost 结果，列出本轮未投票验证者；主动结束的轮次标记未确认结束，
  旧轮记录保留，旧版本未保存详情的轮次明确显示无记录）、
  `BlockAt`/`LatestBlock`、
  `Tx`（queued/proposed/confirmed/replaced/expired/dropped 及区块、替代交易或
  挤出原因关联；被任一未决候选引用的交易显示等待投票且禁止替换）、
  `Account`（已确认序号、待处理交易、最早缺口序号、等待投票/等待打包说明）。

## 稳定编码规则

- 签名编码：前缀 `consensus-lab-tx-v1\n` + canonical JSON（字段按名字典序）：
  `{"content":<base64>,"expiry":<uint64>,"fee":<uint64>,"sender":<base64>,"sequence":<uint64>}`，
  标准 Ed25519 签名。
- 交易标识：上述 JSON 增加 `"signature":<base64>` 后取 SHA-256 十六进制摘要。
- 区块标识：对 `{"round","height","previous_id","transactions"}` 的 canonical JSON
  取 SHA-256；空块交易列表为空数组。
- 轮次提议者：`SHA-256(seed || big-endian(round))` 前 8 字节大端整数对验证者人数取模。

相同种子、初始配置与输入顺序必然产生相同的提议者、交易顺序、区块标识与确认历史。
