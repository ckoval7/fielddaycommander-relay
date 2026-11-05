// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package contact

// bandRanges matches the server's frequency-to-band table, in Hz.
var bandRanges = []struct {
	name      string
	low, high int64
}{
	{"2200m", 135_700, 137_800},
	{"630m", 472_000, 479_000},
	{"160m", 1_800_000, 2_000_000},
	{"80m", 3_500_000, 4_000_000},
	{"40m", 7_000_000, 7_300_000},
	{"20m", 14_000_000, 14_350_000},
	{"15m", 21_000_000, 21_450_000},
	{"10m", 28_000_000, 29_700_000},
	{"6m", 50_000_000, 54_000_000},
	{"2m", 144_000_000, 148_000_000},
	{"1.25m", 222_000_000, 225_000_000},
	{"70cm", 420_000_000, 450_000_000},
	{"33cm", 902_000_000, 928_000_000},
	{"23cm", 1_240_000_000, 1_300_000_000},
	{"13cm", 2_300_000_000, 2_450_000_000},
	{"9cm", 3_300_000_000, 3_500_000_000},
	{"5cm", 5_650_000_000, 5_925_000_000},
	{"3cm", 10_000_000_000, 10_500_000_000},
	{"1.25cm", 24_000_000_000, 24_250_000_000},
	{"6mm", 47_000_000_000, 47_200_000_000},
	{"4mm", 75_500_000_000, 81_000_000_000},
	{"2.5mm", 122_250_000_000, 123_000_000_000},
	{"2mm", 134_000_000_000, 141_000_000_000},
	{"1mm", 241_000_000_000, 250_000_000_000},
}

// BandForHz returns the name of the band a frequency falls in, or "" when it
// falls in none.
func BandForHz(hz int64) string {
	for _, band := range bandRanges {
		if hz >= band.low && hz <= band.high {
			return band.name
		}
	}
	return ""
}
