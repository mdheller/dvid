package annotation

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
)

var (
	syntype datastore.TypeService
	testMu  sync.Mutex
)

// Sets package-level testRepo and TestVersionID
func initTestRepo() (dvid.UUID, dvid.VersionID) {
	testMu.Lock()
	defer testMu.Unlock()
	if syntype == nil {
		var err error
		syntype, err = datastore.TypeServiceByName(TypeName)
		if err != nil {
			log.Fatalf("Can't get synapse type: %s\n", err)
		}
	}
	return datastore.NewTestRepo()
}

func TestSynapseRepoPersistence(t *testing.T) {
	datastore.OpenTest()
	defer datastore.CloseTest()

	uuid, _ := initTestRepo()

	// Make labels and set various properties
	config := dvid.NewConfig()
	dataservice, err := datastore.NewData(uuid, syntype, "synapses", config)
	if err != nil {
		t.Errorf("Unable to create keyvalue instance: %v\n", err)
	}
	data, ok := dataservice.(*Data)
	if !ok {
		t.Errorf("Can't cast data service into synapse.Data\n")
	}
	oldData := *data

	// Restart test datastore and see if datasets are still there.
	if err = datastore.SaveDataByUUID(uuid, data); err != nil {
		t.Fatalf("Unable to save repo during synapse persistence test: %v\n", err)
	}
	datastore.CloseReopenTest()

	dataservice2, err := datastore.GetDataByUUID(uuid, "synapses")
	if err != nil {
		t.Fatalf("Can't get synapse instance from reloaded test db: %v\n", err)
	}
	data2, ok := dataservice2.(*Data)
	if !ok {
		t.Errorf("Returned new data instance 2 is not synapse.Data\n")
	}
	if !oldData.Equals(data2) {
		t.Errorf("Expected %v, got %v\n", oldData, *data2)
	}
}

var testData = Elements{
	{
		Pos:  dvid.Point3d{15, 27, 35}, // Label 1
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{20, 30, 40}}, {Rel: PreSynTo, To: dvid.Point3d{14, 25, 37}}, {Rel: PreSynTo, To: dvid.Point3d{33, 30, 31}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
		Prop: map[string]string{
			"Im a T-Bar":         "yes",
			"I'm not a PSD":      "sure",
			"i'm really special": "",
		},
	},
	{
		Pos:  dvid.Point3d{20, 30, 40}, // Label 2
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1"},
	},
	{
		Pos:  dvid.Point3d{14, 25, 37}, // Label 3
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{33, 30, 31},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{127, 63, 99}, // Label 3
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{88, 47, 80}}, {Rel: PreSynTo, To: dvid.Point3d{120, 65, 100}}, {Rel: PreSynTo, To: dvid.Point3d{126, 67, 98}}},
		Tags: []Tag{"Synapse2"},
		Prop: map[string]string{
			"Im a T-Bar":             "no",
			"I'm not a PSD":          "not really",
			"i'm not really special": "at all",
		},
	},
	{
		Pos:  dvid.Point3d{88, 47, 80}, // Label 4
		Kind: PostSyn,
		Rels: []Relationship{{Rel: GroupedWith, To: dvid.Point3d{14, 25, 37}}, {Rel: PostSynTo, To: dvid.Point3d{127, 63, 99}}, {Rel: GroupedWith, To: dvid.Point3d{20, 30, 40}}},
		Tags: []Tag{"Synapse2"},
	},
	{
		Pos:  dvid.Point3d{120, 65, 100},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{127, 63, 99}}},
		Tags: []Tag{"Synapse2"},
	},
	{
		Pos:  dvid.Point3d{126, 67, 98},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{127, 63, 99}}},
		Tags: []Tag{"Synapse2"},
	},
}

var expectedLabel1 = Elements{
	{
		Pos:  dvid.Point3d{15, 27, 35}, // Label 1
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{20, 30, 40}}, {Rel: PreSynTo, To: dvid.Point3d{14, 25, 37}}, {Rel: PreSynTo, To: dvid.Point3d{33, 30, 31}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
		Prop: map[string]string{
			"Im a T-Bar":         "yes",
			"I'm not a PSD":      "sure",
			"i'm really special": "",
		},
	},
}

var expectedLabel2 = Elements{
	{
		Pos:  dvid.Point3d{20, 30, 40}, // Label 2
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1"},
	},
}

var expectedLabel3 = Elements{
	{
		Pos:  dvid.Point3d{14, 25, 37}, // Label 3
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{127, 63, 99}, // Label 3
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{88, 47, 80}}, {Rel: PreSynTo, To: dvid.Point3d{120, 65, 100}}, {Rel: PreSynTo, To: dvid.Point3d{126, 67, 98}}},
		Tags: []Tag{"Synapse2"},
		Prop: map[string]string{
			"Im a T-Bar":             "no",
			"I'm not a PSD":          "not really",
			"i'm not really special": "at all",
		},
	},
}

var expectedLabel4 = Elements{
	{
		Pos:  dvid.Point3d{88, 47, 80}, // Label 4
		Kind: PostSyn,
		Rels: []Relationship{{Rel: GroupedWith, To: dvid.Point3d{14, 25, 37}}, {Rel: PostSynTo, To: dvid.Point3d{127, 63, 99}}, {Rel: GroupedWith, To: dvid.Point3d{20, 30, 40}}},
		Tags: []Tag{"Synapse2"},
	},
}

var expected3 = Elements{
	{
		Pos:  dvid.Point3d{127, 63, 99},
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{88, 47, 80}}, {Rel: PreSynTo, To: dvid.Point3d{120, 65, 100}}, {Rel: PreSynTo, To: dvid.Point3d{126, 67, 98}}},
		Tags: []Tag{"Synapse2"},
		Prop: map[string]string{
			"Im a T-Bar":             "no",
			"I'm not a PSD":          "not really",
			"i'm not really special": "at all",
		},
	},
}

var afterMove = Elements{
	{
		Pos:  dvid.Point3d{15, 27, 35},
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{20, 30, 40}}, {Rel: PreSynTo, To: dvid.Point3d{14, 25, 37}}, {Rel: PreSynTo, To: dvid.Point3d{33, 30, 31}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
		Prop: map[string]string{
			"Im a T-Bar":         "yes",
			"I'm not a PSD":      "sure",
			"i'm really special": "",
		},
	},
	{
		Pos:  dvid.Point3d{20, 30, 40},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1"},
	},
	{
		Pos:  dvid.Point3d{14, 25, 37},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{33, 30, 31},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{127, 64, 100},
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{88, 47, 80}}, {Rel: PreSynTo, To: dvid.Point3d{120, 65, 100}}, {Rel: PreSynTo, To: dvid.Point3d{126, 67, 98}}},
		Tags: []Tag{"Synapse2"},
		Prop: map[string]string{
			"Im a T-Bar":             "no",
			"I'm not a PSD":          "not really",
			"i'm not really special": "at all",
		},
	},
	{
		Pos:  dvid.Point3d{88, 47, 80},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: GroupedWith, To: dvid.Point3d{14, 25, 37}}, {Rel: PostSynTo, To: dvid.Point3d{127, 64, 100}}, {Rel: GroupedWith, To: dvid.Point3d{20, 30, 40}}},
		Tags: []Tag{"Synapse2"},
	},
	{
		Pos:  dvid.Point3d{120, 65, 100},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{127, 64, 100}}},
		Tags: []Tag{"Synapse2"},
	},
	{
		Pos:  dvid.Point3d{126, 67, 98},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{127, 64, 100}}},
		Tags: []Tag{"Synapse2"},
	},
}

var afterDelete = Elements{
	{
		Pos:  dvid.Point3d{15, 27, 35},
		Kind: PreSyn,
		Rels: []Relationship{{Rel: PreSynTo, To: dvid.Point3d{20, 30, 40}}, {Rel: PreSynTo, To: dvid.Point3d{14, 25, 37}}, {Rel: PreSynTo, To: dvid.Point3d{33, 30, 31}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
		Prop: map[string]string{
			"Im a T-Bar":         "yes",
			"I'm not a PSD":      "sure",
			"i'm really special": "",
		},
	},
	{
		Pos:  dvid.Point3d{20, 30, 40},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1"},
	},
	{
		Pos:  dvid.Point3d{14, 25, 37},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{33, 30, 31},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: PostSynTo, To: dvid.Point3d{15, 27, 35}}},
		Tags: []Tag{"Synapse1", "Zlt90"},
	},
	{
		Pos:  dvid.Point3d{88, 47, 80},
		Kind: PostSyn,
		Rels: []Relationship{{Rel: GroupedWith, To: dvid.Point3d{14, 25, 37}}, {Rel: GroupedWith, To: dvid.Point3d{20, 30, 40}}},
		Tags: []Tag{"Synapse2"},
	},
	{
		Pos:  dvid.Point3d{120, 65, 100},
		Kind: PostSyn,
		Rels: []Relationship{},
		Tags: []Tag{"Synapse2"},
	},
	{
		Pos:  dvid.Point3d{126, 67, 98},
		Kind: PostSyn,
		Rels: []Relationship{},
		Tags: []Tag{"Synapse2"},
	},
}

func getTag(tag Tag, elems Elements) Elements {
	var result Elements
	for _, elem := range elems {
		for _, etag := range elem.Tags {
			if etag == tag {
				result = append(result, elem)
				break
			}
		}
	}
	return result
}

func testResponse(t *testing.T, expected Elements, template string, args ...interface{}) {
	url := fmt.Sprintf(template, args...)
	returnValue := server.TestHTTP(t, "GET", url, nil)
	got := Elements{}
	if err := json.Unmarshal(returnValue, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected.Normalize(), got.Normalize()) {
		t.Errorf("Expected:\n%v\nGot:\n%v\n", expected.Normalize(), got.Normalize())
	}
}

func TestRequests(t *testing.T) {
	datastore.OpenTest()
	defer datastore.CloseTest()

	uuid, _ := initTestRepo()

	config := dvid.NewConfig()
	dataservice, err := datastore.NewData(uuid, syntype, "mysynapses", config)
	if err != nil {
		t.Fatalf("Error creating new data instance: %v\n", err)
	}
	data, ok := dataservice.(*Data)
	if !ok {
		t.Fatalf("Returned new data instance is not synapse.Data\n")
	}

	// PUT first batch of synapses
	testJSON, err := json.Marshal(testData)
	if err != nil {
		t.Fatal(err)
	}
	url1 := fmt.Sprintf("%snode/%s/%s/elements", server.WebAPIPath, uuid, data.DataName())
	server.TestHTTP(t, "POST", url1, strings.NewReader(string(testJSON)))

	// GET synapses back within superset bounding box and make sure all data is there.
	testResponse(t, testData, "%snode/%s/%s/elements/1000_1000_1000/0_0_0", server.WebAPIPath, uuid, data.DataName())

	// Test subset GET
	testResponse(t, expected3, "%snode/%s/%s/elements/5_5_5/126_60_97", server.WebAPIPath, uuid, data.DataName())

	// Test Tag 1
	tag := Tag("Synapse2")
	synapse2 := getTag(tag, testData)
	testResponse(t, synapse2, "%snode/%s/%s/tag/%s", server.WebAPIPath, uuid, data.DataName(), tag)

	// Test Tag 2
	tag2 := Tag("Zlt90")
	zlt90 := getTag(tag2, testData)
	testResponse(t, zlt90, "%snode/%s/%s/tag/%s", server.WebAPIPath, uuid, data.DataName(), tag2)

	// Test move
	url5 := fmt.Sprintf("%snode/%s/%s/move/128_63_99/127_64_100", server.WebAPIPath, uuid, data.DataName())
	server.TestHTTP(t, "POST", url5, nil)
	testResponse(t, afterMove, "%snode/%s/%s/elements/1000_1000_1000/0_0_0", server.WebAPIPath, uuid, data.DataName())

	// --- check tag
	synapse2 = getTag(tag, afterMove)
	testResponse(t, synapse2, "%snode/%s/%s/tag/%s", server.WebAPIPath, uuid, data.DataName(), tag)

	// Test delete
	url6 := fmt.Sprintf("%snode/%s/%s/element/127_64_100", server.WebAPIPath, uuid, data.DataName())
	server.TestHTTP(t, "DELETE", url6, nil)
	testResponse(t, afterDelete, "%snode/%s/%s/elements/1000_1000_1000/0_0_0", server.WebAPIPath, uuid, data.DataName())

	// --- check tag
	synapse2 = getTag(tag, afterDelete)
	testResponse(t, synapse2, "%snode/%s/%s/tag/%s", server.WebAPIPath, uuid, data.DataName(), tag)
}

func TestLabels(t *testing.T) {
	datastore.OpenTest()
	defer datastore.CloseTest()

	// Create testbed volume and data instances
	uuid, _ := initTestRepo()
	var config dvid.Config
	server.CreateTestInstance(t, uuid, "labelblk", "labels", config)
	server.CreateTestInstance(t, uuid, "labelvol", "bodies", config)

	// Establish syncs
	server.CreateTestSync(t, uuid, "labels", "bodies")
	server.CreateTestSync(t, uuid, "bodies", "labels")

	// Populate the labels, which should automatically populate the labelvol
	_ = createLabelTestVolume(t, uuid, "labels")

	if err := BlockOnUpdating(uuid, "bodies"); err != nil {
		t.Fatalf("Error blocking on sync of labels -> bodies: %v\n", err)
	}

	// Add annotations syncing with "labels" instance.
	server.CreateTestInstance(t, uuid, "annotation", "mysynapses", config)
	server.CreateTestSync(t, uuid, "mysynapses", "labels,bodies")

	// PUT first batch of synapses
	testJSON, err := json.Marshal(testData)
	if err != nil {
		t.Fatal(err)
	}
	url1 := fmt.Sprintf("%snode/%s/mysynapses/elements", server.WebAPIPath, uuid)
	server.TestHTTP(t, "POST", url1, strings.NewReader(string(testJSON)))

	// Test if labels were properly denormalized.  For the POST we have synchronized label denormalization.
	// If this were to become asynchronous, we'd want to block on updating like the labelblk<->labelvol sync.

	testResponse(t, expectedLabel1, "%snode/%s/mysynapses/label/1", server.WebAPIPath, uuid)
	testResponse(t, expectedLabel2, "%snode/%s/mysynapses/label/2", server.WebAPIPath, uuid)
	testResponse(t, expectedLabel3, "%snode/%s/mysynapses/label/3", server.WebAPIPath, uuid)
	testResponse(t, expectedLabel4, "%snode/%s/mysynapses/label/4", server.WebAPIPath, uuid)

	// Make change to labelblk and make sure our label synapses have been adjusted

	// Make change to labelvol and make sure our label synapses have been adjusted
}

// A single label block within the volume
type testBody struct {
	label        uint64
	offset, size dvid.Point3d
	blockSpans   dvid.Spans
	voxelSpans   dvid.Spans
}

// A slice of bytes representing 3d label volume
type testVolume struct {
	data []byte
	size dvid.Point3d
}

func newTestVolume(nx, ny, nz int32) *testVolume {
	return &testVolume{
		data: make([]byte, nx*ny*nz*8),
		size: dvid.Point3d{nx, ny, nz},
	}
}

// Sets voxels in body to given label.
func (v *testVolume) add(body testBody, label uint64) {
	nx := v.size[0]
	nxy := nx * v.size[1]
	for _, span := range body.voxelSpans {
		z, y, x0, x1 := span.Unpack()
		p := (z*nxy + y*nx) * 8
		for i := p + x0*8; i <= p+x1*8; i += 8 {
			binary.LittleEndian.PutUint64(v.data[i:i+8], label)
		}
	}
}

// Put label data into given data instance.
func (v *testVolume) put(t *testing.T, uuid dvid.UUID, name string) {
	apiStr := fmt.Sprintf("%snode/%s/%s/raw/0_1_2/%d_%d_%d/0_0_0", server.WebAPIPath,
		uuid, name, v.size[0], v.size[1], v.size[2])
	server.TestHTTP(t, "POST", apiStr, bytes.NewBuffer(v.data))
}

func createLabelTestVolume(t *testing.T, uuid dvid.UUID, name string) *testVolume {
	volume := newTestVolume(128, 128, 128)
	volume.add(body1, 1)
	volume.add(body2, 2)
	volume.add(body3, 3)
	volume.add(body4, 4)

	// Send data over HTTP to populate a data instance
	volume.put(t, uuid, name)
	return volume
}

var (
	bodies = []testBody{
		{
			label:  1,
			offset: dvid.Point3d{10, 10, 30},
			size:   dvid.Point3d{20, 20, 10},
			blockSpans: []dvid.Span{
				{1, 0, 0, 0},
			},
			voxelSpans: []dvid.Span{
				{35, 27, 11, 28}, {36, 28, 13, 25},
			},
		}, {
			label:  2,
			offset: dvid.Point3d{10, 25, 35},
			size:   dvid.Point3d{30, 10, 10},
			blockSpans: []dvid.Span{
				{1, 0, 0, 0},
			},
			voxelSpans: []dvid.Span{
				{40, 30, 12, 20},
			},
		}, {
			label:  3,
			offset: dvid.Point3d{10, 20, 36},
			size:   dvid.Point3d{120, 45, 65},
			blockSpans: []dvid.Span{
				{1, 0, 0, 0},
				{3, 2, 4, 4},
			},
			voxelSpans: []dvid.Span{
				{37, 25, 13, 15}, {99, 63, 126, 127},
			},
		}, {
			label:  4,
			offset: dvid.Point3d{75, 40, 75},
			size:   dvid.Point3d{20, 10, 10},
			blockSpans: []dvid.Span{
				{2, 1, 2, 2},
			},
			voxelSpans: []dvid.Span{
				{80, 47, 87, 89},
			},
		},
	}
	body1 = bodies[0]
	body2 = bodies[1]
	body3 = bodies[2]
	body4 = bodies[3]
)
