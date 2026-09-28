package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	stripe "github.com/stripe/stripe-go/v81"

	"github.com/talyvor/lens/internal/economy"
)

// agent_cards.go — B19.25: a card purchase's capture, reversal or refund settles the agent's balance.
//
// The purchase itself was approved in real time on POST /v1/agent-cards/authorizations (B19.12), which
// debited the agent. What happens to it afterwards arrives here, on the regular webhook:
//
//	issuing_authorization.updated  closed, reversed, expired — or partly reversed while pending
//	issuing_transaction.created    a capture or a refund
//
// and economy.SettleAgentCard settles the hold against it as new append-only ledger rows.

// AgentCardSettler settles an agent card purchase. *economy.DualTokenStore satisfies it.
type AgentCardSettler interface {
	SettleAgentCard(ctx context.Context, st economy.CardSettlement) (economy.CardSettled, error)
}

// WithAgentCards settles agent card purchases from the webhook's Issuing events. Unset, they are acked.
func (s *Service) WithAgentCards(settler AgentCardSettler) *Service {
	s.agentCards = settler
	return s
}

func (s *Service) handleAgentCardSettlement(w http.ResponseWriter, ctx context.Context, event *stripe.Event) {
	if s.agentCards == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	st := economy.CardSettlement{EventID: event.ID, Livemode: event.Livemode, At: time.Unix(event.Created, 0).UTC()}
	switch event.Type {
	case "issuing_authorization.updated":
		var a stripe.IssuingAuthorization
		if err := json.Unmarshal(event.Data.Raw, &a); err != nil || a.ID == "" || a.Card == nil {
			s.log.Warn("billing webhook: unparseable issuing authorization", "event", event.ID)
			w.WriteHeader(http.StatusOK)
			return
		}
		st.Kind, st.AuthorizationID, st.CardID = economy.CardRelease, a.ID, a.Card.ID
		st.Status, st.AmountMinor, st.Currency = string(a.Status), a.Amount, string(a.Currency)
		st.Livemode = st.Livemode || a.Livemode
	case "issuing_transaction.created":
		var t stripe.IssuingTransaction
		if err := json.Unmarshal(event.Data.Raw, &t); err != nil || t.ID == "" || t.Card == nil {
			s.log.Warn("billing webhook: unparseable issuing transaction", "event", event.ID)
			w.WriteHeader(http.StatusOK)
			return
		}
		switch t.Type {
		case stripe.IssuingTransactionTypeCapture:
			st.Kind = economy.CardCapture
		case stripe.IssuingTransactionTypeRefund:
			st.Kind = economy.CardRefund
		default:
			w.WriteHeader(http.StatusOK)
			return
		}
		st.TransactionID, st.CardID, st.AmountMinor, st.Currency = t.ID, t.Card.ID, t.Amount, string(t.Currency)
		if t.Authorization != nil {
			st.AuthorizationID = t.Authorization.ID
		}
		st.Livemode = st.Livemode || t.Livemode
	}
	res, err := s.agentCards.SettleAgentCard(ctx, st)
	if err != nil {
		s.fail(w, "agent card settlement", event.ID, err)
		return
	}
	if res.Recorded {
		s.log.Info("billing webhook: agent card settled", "event", event.ID, "agent", res.AgentID, "amount_ulxc", res.AmountULXC, "why", res.Reason)
	}
	w.WriteHeader(http.StatusOK)
}
