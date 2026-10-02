package credentials

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security
#include <Security/Security.h>
#include <stdlib.h>

// Metadata-only lookup; never reads the password. Refuse ambiguous matches.
static OSStatus study_item(const char *account, const char *service, SecKeychainItemRef *item) {
    CFStringRef a = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
    CFStringRef s = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
    const void *keys[] = {kSecClass, kSecAttrAccount, kSecAttrService, kSecMatchLimit, kSecReturnRef};
    const void *values[] = {kSecClassGenericPassword, a, s, kSecMatchLimitAll, kCFBooleanTrue};
    CFDictionaryRef query = CFDictionaryCreate(NULL, keys, values, 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFTypeRef found = NULL;
    OSStatus status = SecItemCopyMatching(query, &found);
    if (status == errSecSuccess) {
        if (!found || CFGetTypeID(found) != CFArrayGetTypeID() || CFArrayGetCount((CFArrayRef)found) != 1) {
            status = errSecDuplicateItem;
        } else {
            CFTypeRef first = CFArrayGetValueAtIndex((CFArrayRef)found, 0);
            if (CFGetTypeID(first) != SecKeychainItemGetTypeID()) status = errSecInvalidItemRef;
            else *item = (SecKeychainItemRef)CFRetain(first);
        }
    }
    if (found) CFRelease(found);
    CFRelease(query); CFRelease(a); CFRelease(s);
    return status;
}

// Change only the single standard decrypt ACL of this exact password item.
// Keep owner, partition entries, other rights and prompt policy unchanged.
static OSStatus study_authorize_self(const char *account, const char *service) {
    SecKeychainItemRef item = NULL;
    SecAccessRef access = NULL;
    CFArrayRef matching = NULL, oldApps = NULL, selfOnly = NULL;
    CFStringRef description = NULL;
    SecTrustedApplicationRef self = NULL;
    SecKeychainPromptSelector selector = 0;
    OSStatus status = study_item(account, service, &item);
    if (status != errSecSuccess) goto done;
    status = SecKeychainItemCopyAccess(item, &access);
    if (status != errSecSuccess) goto done;
    matching = SecAccessCopyMatchingACLList(access, kSecACLAuthorizationDecrypt);
    if (!matching || CFArrayGetCount(matching) != 1) { status = errSecACLNotSimple; goto done; }
    SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(matching, 0);
    status = SecACLCopyContents(acl, &oldApps, &description, &selector);
    if (status != errSecSuccess) goto done;
    status = SecTrustedApplicationCreateFromPath(NULL, &self);
    if (status != errSecSuccess) goto done;
    const void *apps[] = { self };
    selfOnly = CFArrayCreate(NULL, apps, 1, &kCFTypeArrayCallBacks);
    status = SecACLSetContents(acl, selfOnly, description, selector);
    if (status == errSecSuccess) status = SecKeychainItemSetAccess(item, access);
done:
    if (selfOnly) CFRelease(selfOnly);
    if (self) CFRelease(self);
    if (description) CFRelease(description);
    if (oldApps) CFRelease(oldApps);
    if (matching) CFRelease(matching);
    if (access) CFRelease(access);
    if (item) CFRelease(item);
    return status;
}

static int study_item_locked(const char *account, const char *service) {
    SecKeychainItemRef item = NULL;
    SecKeychainRef keychain = NULL;
    SecKeychainStatus state = 0;
    OSStatus status = study_item(account, service, &item);
    if (status == errSecSuccess) status = SecKeychainItemCopyKeychain(item, &keychain);
    if (status == errSecSuccess) status = SecKeychainGetStatus(keychain, &state);
    if (keychain) CFRelease(keychain);
    if (item) CFRelease(item);
    return status == errSecSuccess && !(state & kSecUnlockStateStatus);
}
*/
import "C"

import (
	"context"
	"errors"
	"unsafe"

	keychain "github.com/keybase/go-keychain"
)

// AuthorizeSelf grants this executable access to one exact existing credential.
// Only call in the explicit interactive setup process, never during collection.
func AuthorizeSelf(ctx context.Context, account, service string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a, s := C.CString(account), C.CString(service)
	defer C.free(unsafe.Pointer(a))
	defer C.free(unsafe.Pointer(s))
	if status := C.study_authorize_self(a, s); status != 0 {
		return keychain.Error(status)
	}
	return nil
}

// FailureResponse exposes codes only, never credential data or native error text.
func FailureResponse(err error, account, service string) Response {
	locked := false
	var status keychain.Error
	if errors.As(err, &status) && (status == keychain.ErrorInteractionNotAllowed || status == keychain.ErrorAuthFailed) {
		a, s := C.CString(account), C.CString(service)
		defer C.free(unsafe.Pointer(a))
		defer C.free(unsafe.Pointer(s))
		locked = C.study_item_locked(a, s) != 0
	}
	return failureResponse(err, locked)
}

func failureResponse(err error, locked bool) Response {
	response := Response{Error: "keychain-error"}
	var status keychain.Error
	if errors.As(err, &status) {
		response.OSStatus = int(status)
	}
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, keychain.ErrorItemNotFound):
		response.Error = "not-found"
	case locked:
		response.Error = "locked"
	case status == keychain.ErrorInteractionNotAllowed:
		response.Error = "interaction-required"
	case status == keychain.ErrorAuthFailed, status == keychain.ErrorNoAccessForItem:
		response.Error = "access-denied"
	case status == keychain.ErrorUserCanceled:
		response.Error = "user-canceled"
	}
	return response
}
