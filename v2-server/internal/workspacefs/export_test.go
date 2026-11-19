package workspacefs

import "io/fs"

// SetBeforeOpenCheckout runs hook between RemoveCheckout's Lstat checks and its open.
func SetBeforeOpenCheckout(hook func(path string)) func() {
	beforeOpenCheckout = hook
	return func() { beforeOpenCheckout = nil }
}

// SetEntryInfo replaces how RemoveCheckout stats one entry while measuring.
func SetEntryInfo(info func(path string, d fs.DirEntry) (fs.FileInfo, error)) func() {
	entryInfo = info
	return func() { entryInfo = func(_ string, d fs.DirEntry) (fs.FileInfo, error) { return d.Info() } }
}

// SetBetweenLstatAndOpen runs hook inside RemoveCheckout's openDir, between its Lstat and its open.
func SetBetweenLstatAndOpen(hook func(name string)) func() {
	betweenLstatAndOpen = hook
	return func() { betweenLstatAndOpen = nil }
}
