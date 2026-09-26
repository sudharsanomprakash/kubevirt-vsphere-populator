package target

import "unsafe"

// uintptrOf returns the address of b's backing array, used only to align
// O_DIRECT buffers.
func uintptrOf(b []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(b))) } //nolint:gosec // address arithmetic for buffer alignment only
