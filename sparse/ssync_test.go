package sparse_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"testing"

	. "github.com/rancher/sparse-tools/sparse"

	"time"

	"github.com/rancher/sparse-tools/log"
)

const batch = 32 // blocks for read/write

type TestFileInterval struct {
	FileInterval
	dataMask byte // XORed with other generated data bytes
}

func (i TestFileInterval) String() string {
	return fmt.Sprintf("{%v %2X}", i.FileInterval, i.dataMask)
}

func TestRandomLayout10MB(t *testing.T) {
	const seed = 0
	const size = 10 /*MB*/ << 20
	prefix := "ssync"
	name := tempFileName(prefix)
	layoutStream := generateLayout(prefix, size, seed)
	layout1, layout2 := teeLayout(layoutStream)

	done := createTestSparseFileLayout(name, size, layout1)
	layoutTmp := unstreamLayout(layout2)
	<-done
	log.Info("Done writing layout of ", len(layoutTmp), "items")

	layout := streamLayout(layoutTmp)
	err := checkTestSparseFileLayout(name, layout)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(name)
}

func TestRandomLayout100MB(t *testing.T) {
	const seed = 0
	const size = 100 /*MB*/ << 20
	prefix := "ssync"
	name := tempFileName(prefix)
	layoutStream := generateLayout(prefix, size, seed)
	layout1, layout2 := teeLayout(layoutStream)

	done := createTestSparseFileLayout(name, size, layout1)
	layoutTmp := unstreamLayout(layout2)
	<-done
	log.Info("Done writing layout of ", len(layoutTmp), "items")

	layout := streamLayout(layoutTmp)
	err := checkTestSparseFileLayout(name, layout)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(name)
}

func TestRandomSync100MB(t *testing.T) {
	const seed = 1
	const size = 100 /*MB*/ << 20
	RandomSync(t, size, seed)
}

func TestRandomSync1GB(t *testing.T) {
	seed := time.Now().UnixNano()
	const size = 1 /*GB*/ << 30
	if testing.Short() {
		t.Skip("skipped 1GB random sync")
	}
	log.Info("seed=", seed)

	log.LevelPush(log.LevelInfo)
	defer log.LevelPop()

	RandomSync(t, size, seed)
}

func RandomSync(t *testing.T, size, seed int64) {
	const localhost = "127.0.0.1"
	const timeout = 10 //seconds
	var remoteAddr = TCPEndPoint{localhost, 5000}
	const srcPrefix = "ssync-src"
	const dstPrefix = "ssync-dst"

	srcName := tempFileName(srcPrefix)
	dstName := tempFileName(dstPrefix)
	srcLayoutStream1, srcLayoutStream2 := teeLayout(generateLayout(srcPrefix, size, seed))
	dstLayoutStream := generateLayout(dstPrefix, size, seed+1)

	srcDone := createTestSparseFileLayout(srcName, size, srcLayoutStream1)
	dstDone := createTestSparseFileLayout(dstName, size, dstLayoutStream)
	srcLayout := unstreamLayout(srcLayoutStream2)
	<-srcDone
	<-dstDone
	log.Info("Done writing layout of ", len(srcLayout), "items")

	log.Info("Syncing...")

	go TestServer(remoteAddr, timeout)
	_, err := SyncFile(srcName, remoteAddr, dstName, timeout)

	if err != nil {
		t.Fatal("sync error")
	}
	log.Info("...syncing done")

	log.Info("Checking...")
	layoutStream := streamLayout(srcLayout)
	err = checkTestSparseFileLayout(dstName, layoutStream)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(srcName)
	os.Remove(dstName)
}

func tempFileName(prefix string) string {
	// Make a temporary file name
	f, err := ioutil.TempFile(".", prefix)
	if err != nil {
		log.Fatal("Failed to make temp file", err)
	}
	defer f.Close()
	return f.Name()
}

func unstreamLayout(in <-chan TestFileInterval) []TestFileInterval {
	layout := make([]TestFileInterval, 0, 4096)
	for i := range in {
		log.Trace("unstream", i)
		layout = append(layout, i)
	}
	return layout
}

func streamLayout(in []TestFileInterval) (out chan TestFileInterval) {
	out = make(chan TestFileInterval, 128)

	go func() {
		for _, i := range in {
			log.Trace("stream", i)
			out <- i
		}
		close(out)
	}()

	return out
}

func teeLayout(in <-chan TestFileInterval) (out1 chan TestFileInterval, out2 chan TestFileInterval) {
	out1 = make(chan TestFileInterval, 128)
	out2 = make(chan TestFileInterval, 128)

	go func() {
		for i := range in {
			log.Trace("Tee1...")
			out1 <- i
			log.Trace("Tee2...")
			out2 <- i
		}
		close(out1)
		close(out2)
	}()

	return out1, out2
}

func generateLayout(prefix string, size, seed int64) <-chan TestFileInterval {
	const maxInterval = 256 // Blocks
	layoutStream := make(chan TestFileInterval, 128)
	r := rand.New(rand.NewSource(seed))

	go func() {
		offset := int64(0)
		for offset < size {
			blocks := int64(r.Intn(maxInterval)) + 1 // 1..maxInterval
			length := blocks * Blocks
			if offset+length > size {
				// don't overshoot size
				length = size - offset
			}

			interval := Interval{offset, offset + length}
			offset += interval.Len()

			kind := SparseHole
			var mask byte
			if r.Intn(2) == 0 {
				// Data
				kind = SparseData
				mask = 0xAA * byte(r.Intn(10)/9) // 10%
			}
			t := TestFileInterval{FileInterval{kind, interval}, mask}
			log.Debug(prefix, t)
			layoutStream <- t
		}
		close(layoutStream)
	}()

	return layoutStream
}

func makeIntervalData(interval TestFileInterval) []byte {
	data := make([]byte, interval.Len())
	if SparseData == interval.Kind {
		for i := range data {
            value := byte((interval.Begin+int64(i))/Blocks)
			data[i] = interval.dataMask ^ value
		}
	}
	return data
}

func createTestSparseFileLayout(name string, fileSize int64, layout <-chan TestFileInterval) (done chan struct{}) {
	done = make(chan struct{})

	// Fill up file with layout data
	go func() {
		f, err := os.Create(name)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		err = f.Truncate(fileSize)
		if err != nil {
			log.Fatal(err)
		}

		for interval := range layout {
			log.Debug("writing...", interval)
			if SparseData == interval.Kind {
				size := batch * Blocks
				for offset := interval.Begin; offset < interval.End; {
					if offset+size > interval.End {
						size = interval.End - offset
					}
					chunkInterval := TestFileInterval{FileInterval{SparseData, Interval{offset, offset + size}}, interval.dataMask}
					data := makeIntervalData(chunkInterval)
					_, err = f.WriteAt(data, offset)
					if err != nil {
						log.Fatal(err)
					}
					offset += size
				}
			}
		}
		f.Sync()
		close(done)
	}()

	return done
}

func checkTestSparseFileLayout(name string, layout <-chan TestFileInterval) error {
	f, err := os.Open(name)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	// Read and check data
	for interval := range layout {
		log.Debug("checking...", interval)
		if SparseData == interval.Kind {
			size := batch * Blocks
			for offset := interval.Begin; offset < interval.End; {
				if offset+size > interval.End {
					size = interval.End - offset
				}
				dataModel := makeIntervalData(TestFileInterval{FileInterval{SparseData, Interval{offset, offset + size}}, interval.dataMask})
				data := make([]byte, size)
				f.ReadAt(data, offset)
				offset += size

				if !bytes.Equal(data, dataModel) {
					return errors.New(fmt.Sprint("data equality check failure at", interval))
				}
			}
		} else if SparseHole == interval.Kind {
			layoutActual, err := RetrieveLayout(f, interval.Interval)
			if err != nil {
				return errors.New(fmt.Sprint("hole retrieval failure at", interval, err))
			}
			if len(layoutActual) != 1 {
				return errors.New(fmt.Sprint("hole check failure at", interval))
			}
			if layoutActual[0] != interval.FileInterval {
				return errors.New(fmt.Sprint("hole equality check failure at", interval))
			}
		}
	}
	return nil // success
}