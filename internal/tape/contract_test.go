package tape

import (
	"encoding/json"
	"os"
	"testing"
)

func TestVolumeFixtureMatchesRecordContract(t *testing.T) {
	b, err := os.ReadFile("../../schemas/fixtures/volume.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Magic   string
		Profile struct {
			Bytes  int `json:"record_bytes"`
			Factor int `json:"tar_blocking_factor"`
		} `json:"tape_profile"`
	}
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Magic != VolumeMagic || v.Profile.Bytes != RecordBytes || v.Profile.Factor != BlockingFactor || v.Profile.Bytes != 512*v.Profile.Factor {
		t.Fatal("volume fixture disagrees with record contract")
	}
}
