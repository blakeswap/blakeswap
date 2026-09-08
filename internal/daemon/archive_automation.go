package daemon

// A policy retains its current offer and any unresolved renewal source in the
// active tier. The normal automation state machine alone can release a proven
// unfilled reservation and bind one successor. Missing archive data never acts
// as permission to refund a charge or start another identity.
func (e *Engine) automationNeedsOffer(id string) bool {
	for _, policy := range e.s.Automations {
		if policy.CurrentOfferID == id {
			return true
		}
		if charge := policy.Charges[id]; charge != nil && charge.State != "committed" && charge.Successor == "" {
			return true
		}
	}
	return false
}
func (e *Engine) automationNeedsReceipt(id string) bool {
	for _, policy := range e.s.Automations {
		if policy.Pending != nil && policy.Pending.RequestID == id {
			return true
		}
	}
	return false
}
