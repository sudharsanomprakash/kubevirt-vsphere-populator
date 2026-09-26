package target

import "unsafe"

func uintptrOf(b []byte) uintptr { return uintptr(unsafe.Pointer(unsafe.SliceData(b))) }
