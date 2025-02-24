package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"

	"github.com/tonindexer/anton/addr"
	"github.com/tonindexer/anton/internal/app"
	"github.com/tonindexer/anton/internal/core"
)

func (s *Service) insertData(
	acc []*core.AccountState,
	msg []*core.Message,
	tx []*core.Transaction,
	b []*core.Block,
) error {
	ctx := context.Background()

	defer app.TimeTrack(time.Now(), "insertData")

	dbTx, err := s.DB.PG.Begin()
	if err != nil {
		return errors.Wrap(err, "cannot begin db tx")
	}
	defer func() {
		_ = dbTx.Rollback()
	}()

	for _, message := range msg {
		err := s.Parser.ParseMessagePayload(ctx, message)
		if errors.Is(err, app.ErrImpossibleParsing) {
			continue
		}
		if err != nil {
			log.Error().Err(err).
				Hex("msg_hash", message.Hash).
				Hex("src_tx_hash", message.SrcTxHash).
				Str("src_addr", message.SrcAddress.String()).
				Hex("dst_tx_hash", message.DstTxHash).
				Str("dst_addr", message.DstAddress.String()).
				Uint32("op_id", message.OperationID).
				Msg("parse message payload")
		}
	}

	if err := s.accountRepo.AddAccountStates(ctx, dbTx, acc); err != nil {
		return errors.Wrap(err, "add account states")
	}
	if err := s.msgRepo.AddMessages(ctx, dbTx, msg); err != nil {
		return errors.Wrap(err, "add messages")
	}
	if err := s.txRepo.AddTransactions(ctx, dbTx, tx); err != nil {
		return errors.Wrap(err, "add transactions")
	}
	if err := s.blockRepo.AddBlocks(ctx, dbTx, b); err != nil {
		return errors.Wrap(err, "add shard block")
	}

	if err := dbTx.Commit(); err != nil {
		return errors.Wrap(err, "cannot commit db tx")
	}

	return nil
}

func (s *Service) uniqAccounts(transactions []*core.Transaction) []*core.AccountState {
	var ret []*core.AccountState

	uniqAcc := make(map[addr.Address]*core.AccountState)

	for j := range transactions {
		tx := transactions[j]
		if tx.Account != nil {
			uniqAcc[tx.Account.Address] = tx.Account
		}
	}

	for _, a := range uniqAcc {
		ret = append(ret, a)
	}

	return ret
}

func (s *Service) addMessage(msg *core.Message, uniqMsg map[string]*core.Message) {
	id := string(msg.Hash)

	for seq, m := range s.unknownDstMsg {
		srcMsg, ok := m[id]
		if !ok {
			continue
		}
		uniqMsg[id] = srcMsg
		delete(m, id)
		if len(m) == 0 {
			delete(s.unknownDstMsg, seq)
		}
	}

	if _, ok := uniqMsg[id]; !ok {
		uniqMsg[id] = msg
		return
	}

	switch {
	case msg.SrcTxLT != 0:
		uniqMsg[id].SrcTxLT, uniqMsg[id].SrcTxHash =
			msg.SrcTxLT, msg.SrcTxHash
		uniqMsg[id].SrcWorkchain, uniqMsg[id].SrcShard, uniqMsg[id].SrcBlockSeqNo =
			msg.SrcWorkchain, msg.SrcShard, msg.SrcBlockSeqNo
		uniqMsg[id].SrcState = msg.SrcState

	case msg.DstTxLT != 0:
		uniqMsg[id].DstTxLT, uniqMsg[id].DstTxHash =
			msg.DstTxLT, msg.DstTxHash
		uniqMsg[id].DstWorkchain, uniqMsg[id].DstShard, uniqMsg[id].DstBlockSeqNo =
			msg.DstWorkchain, msg.DstShard, msg.DstBlockSeqNo
		uniqMsg[id].DstState = msg.DstState
	}
}

func (s *Service) uniqMessages(transactions []*core.Transaction) []*core.Message {
	var ret []*core.Message

	uniqMsg := make(map[string]*core.Message)

	for j := range transactions {
		tx := transactions[j]

		if tx.InMsg != nil {
			s.addMessage(tx.InMsg, uniqMsg)
		}
		for _, out := range tx.OutMsg {
			s.addMessage(out, uniqMsg)
		}
	}

	for _, msg := range uniqMsg {
		if (msg.Type != core.Internal) || (msg.SrcTxLT != 0 && msg.DstTxLT != 0) {
			ret = append(ret, msg)
			continue
		}

		if msg.SrcTxLT == 0 && msg.DstTxLT != 0 {
			if msg.SrcAddress.Workchain() == -1 && msg.DstAddress.Workchain() == -1 {
				continue
			}
			panic(fmt.Errorf("unknown source message with dst tx hash %x on block (%d, %x, %d) from %s to %s",
				msg.DstTxHash, msg.DstWorkchain, msg.DstShard, msg.DstBlockSeqNo, msg.SrcAddress.String(), msg.DstAddress.String()))
		}

		// unknown destination, waiting for next transactions
		if _, ok := s.unknownDstMsg[msg.SrcBlockSeqNo]; !ok {
			s.unknownDstMsg[msg.SrcBlockSeqNo] = make(map[string]*core.Message)
		}
		s.unknownDstMsg[msg.SrcBlockSeqNo][string(msg.Hash)] = msg
	}

	return ret
}

func (s *Service) saveBlocksLoop(results <-chan processedMasterBlock) {
	var (
		insertBlocks []*core.Block
		insertTx     []*core.Transaction
		insertAcc    []*core.AccountState
		insertMsg    []*core.Message
		lastLog      = time.Now()
	)

	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()

	for s.running() {
		var b processedMasterBlock

		select {
		case b = <-results:
		case <-t.C:
			continue
		}

		newMaster := b.master.block

		log.Debug().
			Uint32("master_seq_no", newMaster.SeqNo).
			Int("master_tx", len(newMaster.Transactions)).
			Int("shards", len(b.shards)).
			Msg("new master")

		newBlocks := b.shards
		newBlocks = append(newBlocks, b.master)
		var newTransactions []*core.Transaction

		for i := range newBlocks {
			newBlock := newBlocks[i].block
			insertBlocks = append(insertBlocks, newBlock)
			newTransactions = append(newTransactions, newBlock.Transactions...)
		}

		insertTx = append(insertTx, newTransactions...)
		insertAcc = append(insertAcc, s.uniqAccounts(newTransactions)...)
		insertMsg = append(insertMsg, s.uniqMessages(newTransactions)...)

		if len(s.unknownDstMsg) != 0 {
			continue
		}

		if len(insertBlocks) < s.InsertBlockBatch {
			continue
		}

		if err := s.insertData(insertAcc, insertMsg, insertTx, insertBlocks); err != nil {
			panic(err)
		}
		insertAcc, insertMsg, insertTx, insertBlocks = nil, nil, nil, nil

		lvl := log.Debug()
		if time.Since(lastLog) > time.Minute {
			lvl = log.Info()
			lastLog = time.Now()
		}
		lvl.Uint32("master_seq_no", newMaster.SeqNo).Msg("inserted new block")
	}
}
