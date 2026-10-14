// panelcheck: on-device smoke test of the panel package. Shows a test card, then reads back the displayed
// framebuffer half and compares it with the encoder's output. govee_app must not be drawing (pause it first).
//
//	panelcheck [-hold 5s] [-brightness 100]
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"pixeld/panel"
)

func main() {
	hold := flag.Duration("hold", 5*time.Second, "how long to show the card")
	brightness := flag.Int("brightness", 100, "brightness percent")
	flag.Parse()

	var card panel.Frame
	for y := 0; y < panel.Size; y++ {
		for x := 0; x < panel.Size; x++ {
			switch {
			case x < 6 && y < 6:
				card[y][x] = [3]uint8{255, 0, 0}
			case x >= 26 && y < 6:
				card[y][x] = [3]uint8{0, 255, 0}
			case x < 6 && y >= 26:
				card[y][x] = [3]uint8{0, 0, 255}
			case x >= 26 && y >= 26:
				card[y][x] = [3]uint8{255, 255, 255}
			case x == y || x == 31-y:
				card[y][x] = [3]uint8{0, 200, 200}
			}
		}
	}
	d, err := panel.OpenDisplay("/dev/fb0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer d.Close()
	start := time.Now()
	if err := d.Show(&card, *brightness); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	took := time.Since(start)
	want := panel.Template()
	panel.Encode(want, &card, *brightness)
	diff := 0
	for i, w := range d.Shown() {
		if w != want[i] {
			diff++
		}
	}
	fmt.Printf("shown in %v; readback differs in %d of %d words\n", took, diff, len(want))
	time.Sleep(*hold)
	if diff != 0 {
		os.Exit(2)
	}
}
