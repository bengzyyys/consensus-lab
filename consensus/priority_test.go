package consensus

import (
	"math"
	"testing"
)

// cmpTxPriority 是打包、淘汰、满池接收三处共用的唯一高低关系：
// 费用按 uint64 原生比较，0 与最大值、悬殊费用都不能反转或误判为相同；
// 同费时以完整交易标识字典序定序（更小者优先）。
func TestCmpTxPriority(t *testing.T) {
	const (
		minFee uint64 = 0
		maxFee uint64 = math.MaxUint64
		loFee  uint64 = 1
		hiFee  uint64 = maxFee - 1
	)
	const ida = "aa"
	const idb = "bb"

	cases := []struct {
		name              string
		fee               uint64
		id                string
		otherFee          uint64
		otherID           string
		wantSign          int // 期望结果的符号：1 / 0 / -1
		symmetricWantSign int // 交换两边后的期望符号，应为 wantSign 的相反数
	}{
		{"zero loses to max", minFee, ida, maxFee, ida, -1, 1},
		{"max beats zero", maxFee, ida, minFee, ida, 1, -1},
		{"widely separated fees keep order", loFee, ida, hiFee, idb, -1, 1},
		{"higher fee wins regardless of id", hiFee, idb, loFee, ida, 1, -1},
		{"same fee smaller id wins", loFee, ida, loFee, idb, 1, -1},
		{"same fee larger id loses", loFee, idb, loFee, ida, -1, 1},
		{"same fee same id is equal", loFee, ida, loFee, ida, 0, 0},
		{"max fee tie broken by smaller id", maxFee, ida, maxFee, idb, 1, -1},
		{"zero fee tie broken by smaller id", minFee, ida, minFee, idb, 1, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cmpTxPriority(tc.fee, tc.id, tc.otherFee, tc.otherID)
			if sign(got) != tc.wantSign {
				t.Fatalf("cmpTxPriority(%d,%q,%d,%q) = %d, want sign %d",
					tc.fee, tc.id, tc.otherFee, tc.otherID, got, tc.wantSign)
			}
			rev := cmpTxPriority(tc.otherFee, tc.otherID, tc.fee, tc.id)
			if sign(rev) != tc.symmetricWantSign {
				t.Fatalf("swapped cmp = %d, want sign %d", rev, tc.symmetricWantSign)
			}
		})
	}
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

// 满池接收在费用极值下仍与统一高低关系一致：
// 最大费用新交易挤出零费用旧交易；零费用新交易无法挤出哪怕只收 1 费用的旧交易，
// 同费时只能凭更小标识挤出，标识更大以 pool-full 拒绝。
func TestPriorityExtremesEviction(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 2)

	zero := NewTransaction(keys[0].priv, 1, []byte("zero"), 0, 100)
	one := NewTransaction(keys[1].priv, 1, []byte("one"), 1, 100)
	for _, tx := range []*Transaction{zero, one} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}

	// 最大费用挤出零费用者，费用悬殊也不反转。
	maxTx := NewTransaction(keys[2].priv, 1, []byte("max"), math.MaxUint64, 100)
	res, err := n.Submit(maxTx)
	if err != nil || res.EvictedID != zero.ID() {
		t.Fatalf("max-fee eviction: %+v %v", res, err)
	}

	// 池中现为 one(1) 与 max(MaxUint64)：零费用新交易费用更低，被拒。
	zero2 := NewTransaction(keys[3].priv, 1, []byte("zero2"), 0, 100)
	if _, err := n.Submit(zero2); reason(err) != ReasonPoolFull {
		t.Fatalf("zero-fee tx got %v, want %s", err, ReasonPoolFull)
	}

	// 同费（皆为最大费用）只能凭更小标识挤出；标识更大仍被拒。
	bigA := NewTransaction(keys[0].priv, 2, []byte("bigA"), math.MaxUint64-1, 100)
	if _, err := n.Submit(bigA); err != nil { // 先腾出一个由更低费用占据的位置
		t.Fatalf("setup submit: %v", err)
	}
	bigB := craftTxWithID(t, keys[1].priv, 2, math.MaxUint64-1, bigA.ID(), false)
	if bigB.ID() < bigA.ID() {
		t.Fatal("test setup: bigB id must be larger than bigA id")
	}
	// 池满：可淘汰者是标识更小的 bigA（同费），bigB 标识更大，无法挤出。
	if _, err := n.Submit(bigB); reason(err) != ReasonPoolFull {
		t.Fatalf("same-max-fee larger-id tx got %v, want %s", err, ReasonPoolFull)
	}
}

// 打包在费用极值与费用悬殊下仍取费用最高者；同费按标识，费用为 0 的交易彼此可比较。
func TestPriorityExtremesProposal(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNode(t, keys, 3)

	zero := NewTransaction(keys[0].priv, 1, []byte("zero"), 0, 100)
	maxTx := NewTransaction(keys[1].priv, 1, []byte("max"), math.MaxUint64, 100)
	mid := NewTransaction(keys[2].priv, 1, []byte("mid"), math.MaxUint64/2, 100)
	for _, tx := range []*Transaction{zero, maxTx, mid} {
		if _, err := n.Submit(tx); err != nil {
			t.Fatal(err)
		}
	}
	assertProposalOrder(t, n, []string{maxTx.ID(), mid.ID(), zero.ID()})
}

// 加费替换沿用仅看费用严格更高的规则：零费用可被最大费用替换，
// 已是最大费用的交易无法再加费，同费以 fee-not-higher 拒绝，而非 pool-full。
func TestPriorityExtremesReplacement(t *testing.T) {
	keys := genKeys(t, 4)
	n, _ := newTestNodeCap(t, keys, 10, 1)

	zero := NewTransaction(keys[0].priv, 1, []byte("zero"), 0, 100)
	if _, err := n.Submit(zero); err != nil {
		t.Fatal(err)
	}
	bump := NewTransaction(keys[0].priv, 1, []byte("bump"), math.MaxUint64, 100)
	res, err := n.Submit(bump) // 满池也可替换，不挤出其他交易
	if err != nil || res.ReplacedID != zero.ID() || res.EvictedID != "" {
		t.Fatalf("zero -> max replacement: %+v %v", res, err)
	}
	same := NewTransaction(keys[0].priv, 1, []byte("same"), math.MaxUint64, 100)
	if _, err := n.Submit(same); reason(err) != ReasonLowFee {
		t.Fatalf("max-fee re-replacement got %v, want %s", err, ReasonLowFee)
	}
}
