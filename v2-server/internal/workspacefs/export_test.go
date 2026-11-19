package workspacefs

// SetBeforeOpenCheckout runs hook between RemoveCheckout's Lstat checks and its open.
func SetBeforeOpenCheckout(hook func(path string)) func() {
	beforeOpenCheckout = hook
	return func() { beforeOpenCheckout = nil }
}
