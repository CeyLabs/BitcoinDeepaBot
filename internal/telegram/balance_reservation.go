package telegram

import (
	"sync"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
)

// Payments from one sender may run concurrently, but lnbits only knows the raw
// wallet balance: it cannot see sats reserved in pots, so it would let
// concurrent payments that each passed the available-balance check together
// spend pot funds. Every balance-check-then-pay therefore reserves its sats
// here first, and the next check subtracts what is still reserved.
//
// A reservation only has to cover the gap until lnbits accepts the payment.
// From then on lnbits' own balance already reflects it (a pending outgoing
// payment is deducted immediately), so holding the reservation any longer
// would count the payment twice.

// senderReservations is the in-flight state for one sender.
type senderReservations struct {
	mu       sync.Mutex // serialises balance checks for this sender
	reserved int64      // sats held by payments lnbits has not accepted yet
}

var (
	reservationsMu sync.Mutex
	reservations   = map[int64]*senderReservations{}
)

func reservationsFor(telegramID int64) *senderReservations {
	reservationsMu.Lock()
	defer reservationsMu.Unlock()
	state, ok := reservations[telegramID]
	if !ok {
		state = &senderReservations{}
		reservations[telegramID] = state
	}
	return state
}

// BalanceReservation holds sats for one payment until Release is called.
type BalanceReservation struct {
	state  *senderReservations
	amount int64
	once   sync.Once
}

// Release returns the reserved sats. It is safe to call more than once, so a
// caller can release early and still defer it for the error paths.
func (r *BalanceReservation) Release() {
	r.once.Do(func() {
		r.state.mu.Lock()
		r.state.reserved -= r.amount
		r.state.mu.Unlock()
	})
}

// ReserveBalance reserves need sats of user's available balance (wallet minus
// pots) that no other in-flight payment from user has reserved.
//
// It returns the balance that was available for this payment. The reservation
// is nil when that balance does not cover need; err is set only when the
// balance could not be read. Release the reservation as soon as lnbits has
// accepted or rejected the payment.
func (bot *TipBot) ReserveBalance(user *lnbits.User, need int64) (*BalanceReservation, int64, error) {
	return reservationsFor(user.Telegram.ID).reserve(need, func() (int64, error) {
		return bot.GetUserAvailableBalance(user)
	})
}

// reserve is ReserveBalance with the balance lookup injected.
func (state *senderReservations) reserve(need int64, availableBalance func() (int64, error)) (*BalanceReservation, int64, error) {
	state.mu.Lock()
	defer state.mu.Unlock()

	balance, err := availableBalance()
	if err != nil {
		return nil, 0, err
	}
	available := balance - state.reserved
	if available < 0 {
		available = 0
	}
	if available < need {
		return nil, available, nil
	}
	state.reserved += need
	return &BalanceReservation{state: state, amount: need}, available, nil
}
