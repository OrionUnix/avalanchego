package avalanche

import "github.com/ava-labs/gecko/snow/consensus/snowstorm"

// wrappedTx wraps a snowstorm.Tx.
// This is what gets passed into the consensus instance so that
// if the tx is accepted, it is saved and removed from processing,
// or if it is rejected, it is removed from processing.
type wrappedTx struct {
	t *Transitive
	snowstorm.Tx
}

func (tx *wrappedTx) Accept() error {
	if err := tx.Tx.Accept(); err != nil {
		return err
	}
	tx.t.txManager.UnpinTx(tx.ID())
	return tx.t.SaveTx(tx.Tx)
}

func (tx *wrappedTx) Reject() error {
	tx.t.txManager.UnpinTx(tx.ID())
	return tx.Tx.Reject()
}
