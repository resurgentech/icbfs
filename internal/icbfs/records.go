package icbfs

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/resurgentech/icbfs/internal/pb"
)

// The records kept beside the tree — trash entries, named versions and
// origin records — are Protobuf (spec/proto/icbfs/v1/records.proto), like
// every other object. Ones written before that are JSON, which these
// decoders still read: a JSON record starts with '{', and none of these
// messages can (their first byte is field 1's tag, or there is none).
// The JSON tags on the Go types are that legacy format's names.

// recordLayout is the layout of each record message this package writes.
const recordLayout = 1

func isLegacyJSON(data []byte) bool { return len(data) > 0 && data[0] == '{' }

func checkRecordLayout(kind string, layout uint32) error {
	if layout > recordLayout {
		return fmt.Errorf("%s layout %d: %w", kind, layout, ErrNewerFormat)
	}
	return nil
}

func encodeTrashEntry(e TrashEntry) ([]byte, error) {
	return proto.Marshal(&pb.TrashEntry{
		EntryId: e.EntryID, ItemId: e.ItemID, Type: pb.EntryType(e.Type), Name: e.Name,
		ParentId: e.ParentID, Path: e.Path, DeletedAt: timestampOrNil(e.DeletedAt),
		DeletedBy: e.DeletedBy, Size: e.Size, Version: e.Version, Batch: e.Batch,
		Origin: e.Origin, Pass: e.Pass, Layout: recordLayout,
	})
}

func decodeTrashEntry(data []byte) (TrashEntry, error) {
	var e TrashEntry
	if isLegacyJSON(data) {
		return e, json.Unmarshal(data, &e)
	}
	var m pb.TrashEntry
	if err := proto.Unmarshal(data, &m); err != nil {
		return e, fmt.Errorf("decode trash entry: %w", err)
	}
	if err := checkRecordLayout("trash entry", m.Layout); err != nil {
		return e, err
	}
	return TrashEntry{
		EntryID: m.EntryId, ItemID: m.ItemId, Type: EntryType(m.Type), Name: m.Name,
		ParentID: m.ParentId, Path: m.Path, DeletedAt: timeOrZero(m.DeletedAt),
		DeletedBy: m.DeletedBy, Size: m.Size, Version: m.Version, Batch: m.Batch,
		Origin: m.Origin, Pass: m.Pass,
	}, nil
}

func encodeNamedVersion(nv NamedVersion) ([]byte, error) {
	return proto.Marshal(&pb.NamedVersion{
		Name: nv.Name, Time: timestampOrNil(nv.Time), Scope: nv.Scope,
		CreatedBy: nv.CreatedBy, CreatedAt: timestampOrNil(nv.CreatedAt), Layout: recordLayout,
	})
}

func decodeNamedVersion(data []byte) (NamedVersion, error) {
	var nv NamedVersion
	if isLegacyJSON(data) {
		return nv, json.Unmarshal(data, &nv)
	}
	var m pb.NamedVersion
	if err := proto.Unmarshal(data, &m); err != nil {
		return nv, fmt.Errorf("decode version name: %w", err)
	}
	if err := checkRecordLayout("version name", m.Layout); err != nil {
		return nv, err
	}
	return NamedVersion{
		Name: m.Name, Time: timeOrZero(m.Time), Scope: m.Scope,
		CreatedBy: m.CreatedBy, CreatedAt: timeOrZero(m.CreatedAt),
	}, nil
}

func encodeOriginRecord(r OriginRecord) ([]byte, error) {
	return proto.Marshal(&pb.OriginRecord{
		Drive: r.Drive, Id: r.ID, ItemId: r.ItemID, Version: r.Version, Path: r.Path, Layout: recordLayout,
	})
}

func decodeOriginRecord(data []byte) (OriginRecord, error) {
	var r OriginRecord
	if isLegacyJSON(data) {
		return r, json.Unmarshal(data, &r)
	}
	var m pb.OriginRecord
	if err := proto.Unmarshal(data, &m); err != nil {
		return r, fmt.Errorf("decode origin record: %w", err)
	}
	if err := checkRecordLayout("origin record", m.Layout); err != nil {
		return r, err
	}
	return OriginRecord{Drive: m.Drive, ID: m.Id, ItemID: m.ItemId, Version: m.Version, Path: m.Path}, nil
}
