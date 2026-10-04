package consensus

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
)

// 本文件固定轮次提议者选择的回归预期：对固定种子、固定顺序的验证者名单，
// 指定轮次必须返回 README 公开规则确定的唯一公钥。
// 公开规则：SHA-256(seed || big-endian(round)) 前 8 字节大端整数对名单人数取模。
// 表中的预期公钥十六进制按该规则用独立实现（非本包代码）预先算出并固定，
// 相同输入产生一致结果或两个入口互相一致都不足以通过本组测试。

// fixedValidatorKey 生成确定性的第 i 个验证者公钥（32 字节，内容为 i*32+j+1）。
func fixedValidatorKey(i int) []byte {
	b := make([]byte, 32)
	for j := range b {
		b[j] = byte(i*32 + j + 1)
	}
	return b
}

// fixedValidators 返回 count 个确定性验证者公钥，顺序固定。
func fixedValidators(count int) [][]byte {
	vals := make([][]byte, count)
	for i := range vals {
		vals[i] = fixedValidatorKey(i)
	}
	return vals
}

// wantProposerByRule 在测试侧按 README 公开规则独立复算指定轮次的提议者，
// 不经过被检查的实现，用于交叉核对固定表之外的输入。
func wantProposerByRule(seed []byte, validators [][]byte, round uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], round)
	h := sha256.New()
	h.Write(seed)
	h.Write(buf[:])
	sum := h.Sum(nil)
	return validators[binary.BigEndian.Uint64(sum[:8])%uint64(len(validators))]
}

// newProposerTestNode 用给定种子与验证者名单建节点。
func newProposerTestNode(t *testing.T, seed []byte, validators [][]byte) *Node {
	t.Helper()
	n, err := New(t.TempDir(), Config{Seed: seed, Validators: validators, MaxTxsPerBlock: 4})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// assertProposer 断言 ProposerFor(round) 返回十六进制为 wantHex 的公钥，
// 并与测试侧按公开规则独立复算的结果交叉核对。
func assertProposer(t *testing.T, n *Node, seed []byte, validators [][]byte, round uint64, wantHex string) {
	t.Helper()
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("bad test fixture hex: %v", err)
	}
	got := n.ProposerFor(round)
	if string(got) != string(want) {
		t.Fatalf("ProposerFor(%d) = %x, want pinned %s", round, got, wantHex)
	}
	if string(got) != string(wantProposerByRule(seed, validators, round)) {
		t.Fatalf("ProposerFor(%d) = %x, independent rule computation gives %x", round, got, wantProposerByRule(seed, validators, round))
	}
}

// 固定种子 + 5 名验证者（非二的幂）+ 固定顺序：逐轮钉住提议者公钥。
// 覆盖首轮、255/256 跨字节边界、32 位整数边界与完整 uint64 轮次。
func TestProposerForPinnedKeys(t *testing.T) {
	seed := []byte("proposer-regression-seed-v1")
	validators := fixedValidators(5)
	n := newProposerTestNode(t, seed, validators)

	cases := []struct {
		round uint64
		idx   int // 预期公钥在名单中的位置
		hex   string
	}{
		{1, 1, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"},
		{2, 1, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"},
		{3, 2, "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60"},
		// 255 -> 256 跨越大端编码的字节边界，截断或数值表示变化会在这里选错人。
		{255, 4, "8182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0"},
		{256, 3, "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80"},
		{257, 2, "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60"},
		// 32 位无符号整数边界：轮次必须按完整 uint64 参与选择。
		{4294967295, 1, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"},
		{4294967296, 1, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"},
		{4294967297, 3, "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80"},
		{9223372036854775808, 4, "8182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0"},
		{18446744073709551615, 1, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"},
	}
	for _, c := range cases {
		assertProposer(t, n, seed, validators, c.round, c.hex)
		got := n.ProposerFor(c.round)
		if string(got) != string(validators[c.idx]) {
			t.Fatalf("round %d: proposer is validator at pinned index %d", c.round, c.idx)
		}
	}
}

// 种子中的零字节必须按原字节参与选择，不能在第一个零字节处截断。
// 若实现只保留零字节前的部分（b"\xAA"），各轮会得到另一组公钥
// （如第 1 轮 4142...），本测试固定的预期会立即失败。
func TestProposerSeedWithZeroBytes(t *testing.T) {
	seed := []byte{0xAA, 0x00, 0xBB, 0x00, 0x00, 0xCC}
	validators := fixedValidators(5)
	n := newProposerTestNode(t, seed, validators)

	cases := []struct {
		round uint64
		hex   string
	}{
		{1, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"},
		{2, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"},
		{255, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"},
		{256, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"},
		{4294967297, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"},
	}
	for _, c := range cases {
		assertProposer(t, n, seed, validators, c.round, c.hex)
	}
	// 佐证测试非空转：按截断种子复算的结果确实不同，截断型回归一定会被发现。
	truncated := wantProposerByRule([]byte{0xAA}, validators, 1)
	if string(n.ProposerFor(1)) == string(truncated) {
		t.Fatal("proposer matches truncated-seed computation; zero bytes were dropped")
	}
}

// 名单成员相同但顺序不同：同一索引位置上是不同公钥，
// 两种顺序必须各自返回按公开规则落在自己顺序上的成员，不能忽略调用方给的顺序。
func TestProposerValidatorOrderMatters(t *testing.T) {
	seed := []byte("proposer-regression-seed-v1")
	forward := fixedValidators(5)
	reversed := [][]byte{fixedValidatorKey(4), fixedValidatorKey(3), fixedValidatorKey(2), fixedValidatorKey(1), fixedValidatorKey(0)}

	nFwd := newProposerTestNode(t, seed, forward)
	nRev := newProposerTestNode(t, seed, reversed)

	// 两种顺序下同一轮次钉住的公钥（按各自顺序套用公开规则）。
	cases := []struct {
		round  uint64
		fwdHex string
		revHex string
		fwdIdx int
		revIdx int
	}{
		{1, "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40", "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80", 1, 1},
		{3, "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60", "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60", 2, 2},
		{255, "8182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", 4, 4},
		{256, "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80", "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40", 3, 3},
		{4294967297, "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80", "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40", 3, 3},
	}
	for _, c := range cases {
		assertProposer(t, nFwd, seed, forward, c.round, c.fwdHex)
		assertProposer(t, nRev, seed, reversed, c.round, c.revHex)
		if string(nFwd.ProposerFor(c.round)) != string(forward[c.fwdIdx]) {
			t.Fatalf("round %d: forward order must return member at index %d", c.round, c.fwdIdx)
		}
		if string(nRev.ProposerFor(c.round)) != string(reversed[c.revIdx]) {
			t.Fatalf("round %d: reversed order must return member at index %d", c.round, c.revIdx)
		}
	}
	// 第 1 轮两种顺序落在不同成员上：忽略顺序的实现会在这里露出。
	if string(nFwd.ProposerFor(1)) == string(nRev.ProposerFor(1)) {
		t.Fatal("different validator orders must be able to yield different proposers")
	}
}

// 单人名单：任何合法轮次都返回唯一成员。
func TestProposerSingleValidator(t *testing.T) {
	only := fixedValidatorKey(0)
	n := newProposerTestNode(t, []byte("single-validator-seed"), [][]byte{only})
	for _, round := range []uint64{1, 2, 255, 256, 4294967296, 4294967297, 18446744073709551615} {
		if got := n.ProposerFor(round); string(got) != string(only) {
			t.Fatalf("round %d: proposer = %x, want the only validator %x", round, got, only)
		}
	}
	if got := n.CurrentProposer(); string(got) != string(only) {
		t.Fatalf("CurrentProposer = %x, want the only validator %x", got, only)
	}
}

// 两个查询入口遵守同一约定：新节点 CurrentProposer 与 ProposerFor(1)
// 都返回第一轮钉住的提议者；EndRound 推进轮次后 CurrentProposer 返回新轮次钉住的提议者。
// 该种子下第 1、2 轮提议者是不同成员，轮次不推进的回归无法蒙混。
func TestCurrentProposerFollowsRound(t *testing.T) {
	seed := []byte("round-advance-seed-0")
	validators := fixedValidators(5)
	n := newProposerTestNode(t, seed, validators)

	const round1Hex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	const round2Hex = "8182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0"

	assertProposer(t, n, seed, validators, 1, round1Hex)
	if got := fmt.Sprintf("%x", n.CurrentProposer()); got != round1Hex {
		t.Fatalf("CurrentProposer = %s, want pinned round-1 proposer %s", got, round1Hex)
	}
	if string(n.CurrentProposer()) != string(n.ProposerFor(1)) {
		t.Fatal("CurrentProposer must equal ProposerFor(1) on a new node")
	}

	r, err := n.EndRound()
	if err != nil {
		t.Fatal(err)
	}
	if r != 2 || n.CurrentRound() != 2 {
		t.Fatalf("round after EndRound = %d (CurrentRound %d), want 2", r, n.CurrentRound())
	}
	if got := fmt.Sprintf("%x", n.CurrentProposer()); got != round2Hex {
		t.Fatalf("CurrentProposer after EndRound = %s, want pinned round-2 proposer %s", got, round2Hex)
	}
	assertProposer(t, n, seed, validators, 2, round2Hex)
}

// 查询其他合法轮次只取得该轮结果：当前轮次与本地提议状态都保持原样。
func TestProposerForDoesNotDisturbState(t *testing.T) {
	seed := []byte("proposer-regression-seed-v1")
	validators := fixedValidators(5)
	n := newProposerTestNode(t, seed, validators)

	// 尚未产生本地提议时查询其他轮次。
	assertProposer(t, n, seed, validators, 7, "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60")
	if n.CurrentRound() != 1 {
		t.Fatalf("CurrentRound = %d after querying round 7, want 1", n.CurrentRound())
	}
	if _, ok := n.Proposal(); ok {
		t.Fatal("querying another round must not create a local proposal")
	}

	// 已有本地提议后查询过去与未来的轮次，提议与轮次都不变。
	p, err := n.Propose()
	if err != nil {
		t.Fatal(err)
	}
	assertProposer(t, n, seed, validators, 3, "4142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f60")
	assertProposer(t, n, seed, validators, 4294967297, "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80")
	if n.CurrentRound() != 1 {
		t.Fatalf("CurrentRound = %d, want 1", n.CurrentRound())
	}
	p2, ok := n.Proposal()
	if !ok || p2.BlockID != p.BlockID {
		t.Fatalf("local proposal changed after querying other rounds: %+v", p2)
	}
}

// 返回的公钥是调用方手中的副本：改写它不影响后续查询结果。
func TestProposerReturnedKeyIsCopy(t *testing.T) {
	seed := []byte("proposer-regression-seed-v1")
	validators := fixedValidators(5)
	n := newProposerTestNode(t, seed, validators)

	const round1Hex = "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40"
	got := n.ProposerFor(1)
	if fmt.Sprintf("%x", got) != round1Hex {
		t.Fatalf("ProposerFor(1) = %x, want %s", got, round1Hex)
	}
	// 改写返回副本的每一个字节。
	for i := range got {
		got[i] ^= 0xff
	}
	cur := n.CurrentProposer()
	cur[0] ^= 0xff

	assertProposer(t, n, seed, validators, 1, round1Hex)
	if fmt.Sprintf("%x", n.CurrentProposer()) != round1Hex {
		t.Fatal("mutating a returned proposer key corrupted later queries")
	}
	// 名单本身也不受影响：其他轮次仍按原名单选择。
	assertProposer(t, n, seed, validators, 256, "6162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80")
}
