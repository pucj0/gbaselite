package storage

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
)

const pageIndexBuildMemory = 8 << 20

func (p *Persistence) buildTableDiskIndexes(databaseName string, table TableSnapshot, refs *tablePageRefs) error {
	var previous *tablePageRefs
	if p.pages.manifest != nil {
		for databaseIndex, database := range p.pages.manifest.Snapshot.Databases {
			if normalizeName(database.Name) != normalizeName(databaseName) {
				continue
			}
			for tableIndex, old := range database.Tables {
				if normalizeName(old.Name) == normalizeName(table.Name) {
					previous = &p.pages.manifest.Databases[databaseIndex].Tables[tableIndex]
					break
				}
			}
		}
	}
	refs.Indexes = make([]DiskIndexDescriptor, 0, len(table.Indexes))
	refs.IndexFingerprints = make([][32]byte, 0, len(table.Indexes))
	columnPositions := make(map[string]int, len(table.Columns))
	for index, column := range table.Columns {
		columnPositions[normalizeName(column.Name)] = index
	}
	for _, definition := range table.Indexes {
		definitionBytes, err := pageGob(definition)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, _ = hash.Write(definitionBytes)
		positions := make([]int, len(definition.Columns))
		for index, name := range definition.Columns {
			position, ok := columnPositions[normalizeName(name)]
			if !ok {
				return fmt.Errorf("index %q refers to missing column %q", definition.Name, name)
			}
			positions[index] = position
		}
		key := make(Row, len(positions))
		encoded := make([]byte, 0, 256)
		for rowPosition, row := range table.Rows {
			for index, position := range positions {
				key[index] = row[position]
			}
			encoded, err = appendDiskKey(encoded[:0], key)
			if err != nil {
				return fmt.Errorf("index %q: %w", definition.Name, err)
			}
			encoded = appendDiskUint(encoded, uint64(rowPosition))
			_, _ = hash.Write(encoded)
		}
		var fingerprint [32]byte
		copy(fingerprint[:], hash.Sum(nil))
		var descriptor DiskIndexDescriptor
		reused := false
		if previous != nil {
			for index, old := range previous.Indexes {
				if normalizeName(old.Definition.Name) == normalizeName(definition.Name) && index < len(previous.IndexFingerprints) && previous.IndexFingerprints[index] == fingerprint {
					descriptor, reused = old, true
					break
				}
			}
		}
		if !reused {
			descriptor, err = BuildDiskIndex(filepath.Join(p.pages.directory, "disk-indexes"), definition, table.Columns, func(yield func(int, Row) error) error {
				for position, row := range table.Rows {
					if err := yield(position, row); err != nil {
						return err
					}
				}
				return nil
			}, pageIndexBuildMemory)
			if err != nil {
				return err
			}
			if err := syncPageDirectory(filepath.Join(p.pages.directory, "disk-indexes")); err != nil {
				return err
			}
			if err := syncPageDirectory(p.pages.directory); err != nil {
				return err
			}
			p.pages.stats.DiskIndexBuilds++
			p.pages.stats.DiskIndexBytesWritten += uint64(descriptor.Bytes)
		}
		refs.Indexes = append(refs.Indexes, descriptor)
		refs.IndexFingerprints = append(refs.IndexFingerprints, fingerprint)
	}
	return nil
}
