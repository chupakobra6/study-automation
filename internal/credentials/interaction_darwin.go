package credentials

/*
#cgo LDFLAGS: -framework Security
#include <Security/Security.h>
static OSStatus study_keychain_interaction(int allowed) {
    return SecKeychainSetUserInteractionAllowed(allowed != 0);
}
*/
import "C"

import keychain "github.com/keybase/go-keychain"

// ConfigureInteraction applies to this small credential process only. Background
// callers never display Keychain UI; authorization is an explicit setup action.
func ConfigureInteraction(allowed bool) error {
	value := 0
	if allowed {
		value = 1
	}
	if status := C.study_keychain_interaction(C.int(value)); status != 0 {
		return keychain.Error(status)
	}
	return nil
}
