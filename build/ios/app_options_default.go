//go:build !ios

package main

import "github.com/wailsapp/wails/v3/pkg/application"

// modifyOptionsForIOS is a no-op on non-iOS platforms
//
//lint:ignore U1000 no-op stub required by the ios build tag
func modifyOptionsForIOS(opts *application.Options) {
	// No modifications needed for non-iOS platforms
}
