package consensus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// save 将状态原子写入状态文件：先写临时文件并 fsync，再 rename 覆盖，
// 最后 fsync 目录。崩溃只会留下旧文件或新文件之一，不会出现半截状态。
func (n *Node) save(st *state) error {
	if n.injectSaveErr != nil {
		return n.injectSaveErr
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir := n.dir
	tmp := filepath.Join(dir, stateFileName+".tmp")
	final := filepath.Join(dir, stateFileName)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit state: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// clone 深拷贝完整状态，使一次确认可以先在副本上完整应用、原子落盘成功后再替换内存。
func (s *state) clone() *state {
	c := &state{
		Version:   s.Version,
		Seed:      append([]byte(nil), s.Seed...),
		MaxTxs:    s.MaxTxs,
		Round:     s.Round,
		LastBlock: s.LastBlock,
		Accounts:  make(map[string]uint64, len(s.Accounts)),
		Pool:      make(map[string]map[uint64]string, len(s.Pool)),
		Entries:   make(map[string]*poolEntry, len(s.Entries)),
		Confirmed: make(map[string]confirmedRef, len(s.Confirmed)),
		Blocks:    make([]Block, len(s.Blocks)),
	}
	c.Validators = make([][]byte, len(s.Validators))
	for i, v := range s.Validators {
		c.Validators[i] = append([]byte(nil), v...)
	}
	for k, v := range s.Accounts {
		c.Accounts[k] = v
	}
	for sender, seqs := range s.Pool {
		m := make(map[uint64]string, len(seqs))
		for seq, id := range seqs {
			m[seq] = id
		}
		c.Pool[sender] = m
	}
	for id, e := range s.Entries {
		c.Entries[id] = &poolEntry{
			Tx:         copyTx(e.Tx),
			Status:     e.Status,
			ReplacedBy: e.ReplacedBy,
		}
	}
	for id, ref := range s.Confirmed {
		c.Confirmed[id] = ref
	}
	for i, b := range s.Blocks {
		c.Blocks[i] = Block{
			Height:     b.Height,
			Round:      b.Round,
			ID:         b.ID,
			PreviousID: b.PreviousID,
			TxIDs:      append([]string(nil), b.TxIDs...),
		}
	}
	if s.Proposal != nil {
		c.Proposal = &proposalState{
			Round:   s.Proposal.Round,
			TxIDs:   append([]string(nil), s.Proposal.TxIDs...),
			BlockID: s.Proposal.BlockID,
			Votes:   append([]string(nil), s.Proposal.Votes...),
		}
	}
	for _, cand := range s.Candidates {
		c.Candidates = append(c.Candidates, &candidateState{
			BlockID: cand.BlockID,
			TxIDs:   append([]string(nil), cand.TxIDs...),
			Votes:   append([]string(nil), cand.Votes...),
			Local:   cand.Local,
		})
	}
	for _, h := range s.History {
		rec := &roundRecord{
			Round:      h.Round,
			Ended:      h.Ended,
			WonBlockID: h.WonBlockID,
		}
		for _, cr := range h.Candidates {
			rec.Candidates = append(rec.Candidates, &candidateRecord{
				BlockID: cr.BlockID,
				TxIDs:   append([]string(nil), cr.TxIDs...),
				Votes:   append([]string(nil), cr.Votes...),
				Local:   cr.Local,
				Status:  cr.Status,
			})
		}
		c.History = append(c.History, rec)
	}
	return c
}
