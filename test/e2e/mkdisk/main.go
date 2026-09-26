// Command mkdisk writes a deterministic, partly sparse raw disk image for tests.
package main

import (
	"flag"
	"log"
	"math/rand/v2"
	"os"
)

func main() {
	size := flag.Int64("size", 0, "size in bytes (multiple of 512)")
	seed := flag.Uint64("seed", 1, "random seed")
	out := flag.String("o", "", "output file")
	flag.Parse()
	if *size <= 0 || *size%512 != 0 || *out == "" {
		log.Fatal("usage: mkdisk -size N -o file [-seed S]")
	}
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(*size); err != nil {
		log.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(*seed, *seed^0xdeadbeef))
	buf := make([]byte, 1<<20)
	for off := int64(0); off < *size; off += int64(len(buf)) {
		n := min(int64(len(buf)), *size-off)
		switch rng.IntN(4) {
		case 0: // leave a hole
			continue
		case 1: // random data
			for i := range buf[:n] {
				buf[i] = byte(rng.Uint32())
			}
		case 2: // a pattern
			for i := range buf[:n] {
				buf[i] = byte((off + int64(i)) / 4096)
			}
		case 3: // mostly zero with a few non-zero sectors
			clear(buf[:n])
			for j := 0; j < 4; j++ {
				s := int64(rng.IntN(int(n/512))) * 512
				for i := s; i < s+512; i++ {
					buf[i] = 0x5a
				}
			}
		}
		if _, err := f.WriteAt(buf[:n], off); err != nil {
			log.Fatal(err)
		}
	}
}
