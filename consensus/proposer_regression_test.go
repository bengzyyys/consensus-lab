package consensus

import (
	"bytes"
	"math"
	"testing"
)

// 本文件固定轮次提议者选择的回归金色值。
// 公开规则（README）：SHA-256(seed || big-endian(round)) 前 8 字节大端整数
// 对验证者人数取模，选中名单该位置的成员。
// 下表中的期望下标均按该规则用独立实现（Python hashlib）预先算出并硬编码，
// 不经过被检查的 proposerAt，因此能抓住“始终稳定但选错人”的回归。

// goldenValidators 返回 5 个确定的验证者公钥：第 i 个为 32 字节全 i+1。
// 5 不是 2 的幂，覆盖人数非 2 的幂时的取模行为。
func goldenValidators() [][]byte {
	vals := make([][]byte, 5)
	for i := range vals {
		vals[i] = bytes.Repeat([]byte{byte(i + 1)}, 32)
	}
	return vals
}

func newGoldenNode(t *testing.T, seed []byte, vals [][]byte) *Node {
	t.Helper()
	n, err := New(t.TempDir(), Config{Seed: seed, Validators: vals, MaxTxsPerBlock: 1})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// goldenSeed 是主测试种子。
var goldenSeed = []byte("proposer-regression-seed")

// goldenRounds 固定 goldenSeed + 5 验证者下各轮次的期望名单下标，
// 覆盖：普通轮次、255/256 跨字节边界、超过 32 位整数范围的合法轮次，
// 以及 uint64 最大值。轮次必须按完整 uint64 参与计算。
var goldenRounds = []struct {
	round uint64
	want  int // 期望选中的验证者名单下标
}{
	{1, 0},
	{2, 3},
	{3, 4},
	{255, 0},
	{256, 2},
	{257, 2},
	{1<<32 - 1, 2},
	{1 << 32, 4},
	{1<<32 + 1, 3},
	{math.MaxUint64, 3},
}

// 指定种子、名单与轮次必须返回金色公钥本身，而不仅是“在名单内”或“多次一致”。
func TestProposerGoldenVectors(t *testing.T) {
	vals := goldenValidators()
	n := newGoldenNode(t, goldenSeed, vals)
	for _, tc := range goldenRounds {
		got := n.ProposerFor(tc.round)
		if !bytes.Equal(got, vals[tc.want]) {
			t.Fatalf("ProposerFor(%d) = %x, want validators[%d] = %x", tc.round, got, tc.want, vals[tc.want])
		}
	}
}

// 名单原始顺序影响结果：成员相同、排列不同的名单，在同一种子与轮次下
// 应返回各自名单中被选中下标位置的成员，不能忽略调用方提供的顺序。
func TestProposerGoldenValidatorOrder(t *testing.T) {
	vals := goldenValidators()
	// 与 goldenRounds 相同的种子与下标：排列后的名单在下标 idx 处的成员不同。
	permutations := []struct {
		name  string
		order []int // 新名单第 i 位是原始名单第 order[i] 个成员
	}{
		{"reversed", []int{4, 3, 2, 1, 0}},
		{"rotated", []int{2, 3, 4, 0, 1}},
	}
	rounds := []struct {
		round uint64
		want  int // 与 goldenRounds 一致的金色下标
	}{
		{1, 0},
		{2, 3},
		{256, 2},
	}
	for _, p := range permutations {
		perm := make([][]byte, len(p.order))
		for i, o := range p.order {
			perm[i] = vals[o]
		}
		n := newGoldenNode(t, goldenSeed, perm)
		for _, tc := range rounds {
			got := n.ProposerFor(tc.round)
			want := perm[tc.want]
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: ProposerFor(%d) = %x, want permuted validators[%d] = %x",
					p.name, tc.round, got, tc.want, want)
			}
		}
	}
}

// 种子按原始字节参与选择：包含零字节时不能截断（只保留零字节前的部分），
// 前导零字节也不能丢弃。下列金色下标在错误截断时会得到不同结果：
// {01,00,02,00,03} 若截断为 {01}，第 3 轮会错选下标 4、第 256 轮会错选下标 4；
// {00,07} 若丢掉前导零变为 {07}，第 1 轮会错选下标 1。
func TestProposerGoldenSeedWithZeroBytes(t *testing.T) {
	vals := goldenValidators()
	cases := []struct {
		seed   []byte
		rounds []struct {
			round uint64
			want  int
		}
	}{
		{[]byte{0x01, 0x00, 0x02, 0x00, 0x03}, []struct {
			round uint64
			want  int
		}{{1, 1}, {2, 0}, {3, 0}, {5, 2}, {256, 2}}},
		{[]byte{0x00, 0x07}, []struct {
			round uint64
			want  int
		}{{1, 4}, {2, 1}, {256, 4}}},
	}
	for _, tc := range cases {
		n := newGoldenNode(t, tc.seed, vals)
		for _, r := range tc.rounds {
			got := n.ProposerFor(r.round)
			if !bytes.Equal(got, vals[r.want]) {
				t.Fatalf("seed %x: ProposerFor(%d) = %x, want validators[%d] = %x",
					tc.seed, r.round, got, r.want, vals[r.want])
			}
		}
	}
}

// 名单只有一个成员时，任意合法轮次都返回该唯一成员。
func TestProposerGoldenSingleValidator(t *testing.T) {
	vals := goldenValidators()
	n := newGoldenNode(t, goldenSeed, vals[:1])
	for _, round := range []uint64{1, 2, 3, 255, 256, 1<<32 + 1, math.MaxUint64} {
		if got := n.ProposerFor(round); !bytes.Equal(got, vals[0]) {
			t.Fatalf("single validator: ProposerFor(%d) = %x, want %x", round, got, vals[0])
		}
		if got := n.CurrentProposer(); !bytes.Equal(got, vals[0]) {
			t.Fatalf("single validator: CurrentProposer() = %x, want %x", got, vals[0])
		}
	}
}

// 两个查询入口与轮次推进遵守同一约定：
// 新节点 CurrentProposer 与 ProposerFor(1) 都是第一轮的金色提议者；
// EndRound 后 CurrentProposer 是新轮次的金色提议者；
// 查询其他合法轮次只取得该轮结果，不改变当前轮次与本地提议状态；
// 返回的公钥是副本，改写后再次查询仍得到正确公钥。
func TestProposerQueryContract(t *testing.T) {
	vals := goldenValidators()
	n := newGoldenNode(t, goldenSeed, vals)

	// 新节点：两个入口都给出第 1 轮金色提议者（validators[0]）。
	if got := n.CurrentProposer(); !bytes.Equal(got, vals[0]) {
		t.Fatalf("new node CurrentProposer() = %x, want %x", got, vals[0])
	}
	if got := n.ProposerFor(1); !bytes.Equal(got, vals[0]) {
		t.Fatalf("new node ProposerFor(1) = %x, want %x", got, vals[0])
	}

	// 查询其他轮次（含尚未到达的轮次）不改变当前轮次，也不产生本地提议。
	if got := n.ProposerFor(256); !bytes.Equal(got, vals[2]) {
		t.Fatalf("ProposerFor(256) = %x, want %x", got, vals[2])
	}
	if r := n.CurrentRound(); r != 1 {
		t.Fatalf("querying another round changed current round to %d", r)
	}
	if _, ok := n.Proposal(); ok {
		t.Fatal("querying another round must not create a local proposal")
	}

	// 已有本地提议时查询其他轮次：提议与轮次都保持原样。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	n.ProposerFor(1<<32 + 1)
	if r := n.CurrentRound(); r != 1 {
		t.Fatalf("querying another round changed current round to %d", r)
	}
	p2, ok := n.Proposal()
	if !ok || p2.BlockID != p.BlockID {
		t.Fatal("querying another round must not change the local proposal")
	}

	// 主动结束当前轮次后，CurrentProposer 是第 2 轮的金色提议者（validators[3]）。
	next, err := n.EndRound()
	if err != nil {
		t.Fatal(err)
	}
	if next != 2 {
		t.Fatalf("EndRound returned round %d, want 2", next)
	}
	if got := n.CurrentProposer(); !bytes.Equal(got, vals[3]) {
		t.Fatalf("round 2 CurrentProposer() = %x, want %x", got, vals[3])
	}
	if got := n.ProposerFor(2); !bytes.Equal(got, vals[3]) {
		t.Fatalf("ProposerFor(2) = %x, want %x", got, vals[3])
	}

	// 返回的公钥是调用方的副本：改写其中的字节不影响后续查询结果。
	got := n.CurrentProposer()
	for i := range got {
		got[i] ^= 0xff
	}
	again := n.CurrentProposer()
	if !bytes.Equal(again, vals[3]) {
		t.Fatalf("after mutating returned key, CurrentProposer() = %x, want %x", again, vals[3])
	}
	pf := n.ProposerFor(2)
	pf[0] ^= 0xff
	if again := n.ProposerFor(2); !bytes.Equal(again, vals[3]) {
		t.Fatalf("after mutating returned key, ProposerFor(2) = %x, want %x", again, vals[3])
	}
}
