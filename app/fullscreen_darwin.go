//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

// Wails only marks the window fullscreen-capable when it starts fullscreen,
// so the green title-bar button zooms instead of entering fullscreen. Add the
// behaviour to every app window once the window exists.
static void enableFullScreenButton(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		for (NSWindow *w in [NSApp windows]) {
			[w setCollectionBehavior:[w collectionBehavior] | NSWindowCollectionBehaviorFullScreenPrimary];
		}
	});
}
*/
import "C"

func enableFullScreenButton() { C.enableFullScreenButton() }
