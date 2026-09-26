package worker

import (
	"encoding/json"
	"os"
)

// Termination is the compact result written to the container termination
// message (limited by Kubernetes to 4096 bytes). The controller reads it to
// decide whether a population was validated.
type Termination struct {
	Validated         bool    `json:"validated"`
	Phase             string  `json:"phase,omitempty"`
	Error             string  `json:"error,omitempty"`
	VirtualSize       int64   `json:"virtualSize,omitempty"`
	GuestDigest       string  `json:"guestDigest,omitempty"`
	DestinationDigest string  `json:"destinationDigest,omitempty"`
	ExportBytes       int64   `json:"exportBytes,omitempty"`
	ExportSHA256      string  `json:"exportSHA256,omitempty"`
	ManifestStatus    string  `json:"manifestStatus,omitempty"`
	LeaseKey          string  `json:"leaseKey,omitempty"`
	VMMoRef           string  `json:"vmMoRef,omitempty"`
	DiskKey           int32   `json:"diskKey,omitempty"`
	ServerVersion     string  `json:"serverVersion,omitempty"`
	QemuImgVersion    string  `json:"qemuImgVersion,omitempty"`
	ExportSeconds     float64 `json:"exportSeconds,omitempty"`
	ExportBytesPerSec float64 `json:"exportBytesPerSecond,omitempty"`
	ConvertSeconds    float64 `json:"convertSeconds,omitempty"`
	VerifySeconds     float64 `json:"verifySeconds,omitempty"`
	TotalSeconds      float64 `json:"totalSeconds,omitempty"`
}

// Compact builds the termination message for a result.
func (r *Result) Compact() Termination {
	t := Termination{
		Validated: r.Validated, Phase: r.Phase, Error: r.Error, VirtualSize: r.VirtualSize,
		GuestDigest: r.GuestDigest, DestinationDigest: r.DestinationDigest,
		ExportBytes: r.Export.Bytes, ExportSHA256: r.Export.SHA256, ManifestStatus: r.Manifest.Status,
		LeaseKey: r.Disk.LeaseKey, VMMoRef: r.VM.MoRef, DiskKey: r.Disk.Key, ServerVersion: r.Server.Version,
		QemuImgVersion: r.Conversion.Version, ExportSeconds: r.Export.Seconds, ExportBytesPerSec: r.Export.BytesPerSecond,
		ConvertSeconds: r.Conversion.Seconds, VerifySeconds: r.Destination.Seconds, TotalSeconds: r.TotalSeconds,
	}
	if len(t.Error) > 1500 {
		t.Error = t.Error[:1500] + "..."
	}
	return t
}

// WriteTermination writes the compact result to path (typically
// /dev/termination-log). Errors are returned but are not fatal to callers.
func WriteTermination(path string, r *Result) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(r.Compact())
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
