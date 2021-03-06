package mbtiles

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type TileFormat uint8

const (
	UNKNOWN TileFormat = iota
	GZIP               // encoding = gzip
	ZLIB               // encoding = deflate
	PNG
	JPG
	PBF
	WEBP
)

func (t TileFormat) String() string {
	switch t {
	case PNG:
		return "png"
	case JPG:
		return "jpg"
	case PBF:
		return "pbf"
	case WEBP:
		return "webp"
	default:
		return ""
	}
}

func (t TileFormat) ContentType() string {
	switch t {
	case PNG:
		return "image/png"
	case JPG:
		return "image/jpeg"
	case PBF:
		return "application/x-protobuf" // Content-Encoding header must be gzip
	case WEBP:
		return "image/webp"
	default:
		return ""
	}
}

type DB struct {
	filename           string
	db                 *sql.DB
	tileformat         TileFormat // tile format: PNG, JPG, PBF
	timestamp          time.Time  // timestamp of file, for cache control headers
	hasUTFGrid         bool
	utfgridCompression TileFormat
	hasUTFGridData     bool
}

// Creates a new DB instance.
// Connection is closed by runtime on application termination or by calling .Close() method.
func NewDB(filename string) (*DB, error) {
	_, id := filepath.Split(filename)
	id = strings.Split(id, ".")[0]

	db, err := sql.Open("sqlite3", filename)
	if err != nil {
		return nil, err
	}

	//Saves last modified mbtiles time for setting Last-Modified header
	fileStat, err := os.Stat(filename)
	if err != nil {
		return nil, fmt.Errorf("could not read file stats for mbtiles file: %s\n", filename)
	}

	//query a sample tile to determine format
	var data []byte
	err = db.QueryRow("select tile_data from tiles limit 1").Scan(&data)
	if err != nil {
		return nil, err
	}
	tileformat, err := detectTileFormat(&data)
	if err != nil {
		return nil, err
	}
	if tileformat == GZIP {
		tileformat = PBF // GZIP masks PBF, which is only expected type for tiles in GZIP format
	}
	out := DB{
		db:         db,
		tileformat: tileformat,
		timestamp:  fileStat.ModTime().Round(time.Second), // round to nearest second
	}

	// UTFGrids
	// first check to see if requisite tables exist
	var count int
	err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='view' AND name = 'grids'").Scan(&count)
	if err != nil {
		return nil, err
	}
	if count == 1 {
		// query a sample grid to detect type
		var gridData []byte
		err = db.QueryRow("select grid from grids where grid is not null LIMIT 1").Scan(&gridData)
		if err != nil {
			if err != sql.ErrNoRows {
				return nil, fmt.Errorf("could not read sample grid to determine type: %v", err)
			}
		} else {
			out.hasUTFGrid = true
			out.utfgridCompression, err = detectTileFormat(&gridData)
			if err != nil {
				return nil, fmt.Errorf("could not determine UTF Grid compression type: %v", err)
			}

			// Check to see if grid_data view exists
			count = 0 // prevent use of prior value
			err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='view' AND name = 'grid_data'").Scan(&count)
			if err != nil {
				return nil, err
			}
			if count == 1 {
				out.hasUTFGridData = true
			}
		}
	}

	return &out, nil

}

// Reads a grid at z, x, y into provided *[]byte.
func (tileset *DB) ReadTile(z uint8, x uint64, y uint64, data *[]byte) error {
	err := tileset.db.QueryRow("select tile_data from tiles where zoom_level = ? and tile_column = ? and tile_row = ?", z, x, y).Scan(data)
	if err != nil {
		if err == sql.ErrNoRows {
			*data = nil // not a problem, just return empty bytes
			return nil
		}
		return err
	}
	return nil
}

// Reads a grid at z, x, y into provided *[]byte.
// This merges in grid key data, if any exist
// The data is returned in the original compression encoding (zlib or gzip)
func (tileset *DB) ReadGrid(z uint8, x uint64, y uint64, data *[]byte) error {
	if !tileset.hasUTFGrid {
		return errors.New("Tileset does not contain UTFgrids")
	}

	err := tileset.db.QueryRow("select grid from grids where zoom_level = ? and tile_column = ? and tile_row = ?", z, x, y).Scan(data)
	if err != nil {
		if err == sql.ErrNoRows {
			*data = nil // not a problem, just return empty bytes
			return nil
		}
		return err
	}

	if tileset.hasUTFGridData {
		keydata := make(map[string]interface{})
		var (
			key   string
			value []byte
		)

		rows, err := tileset.db.Query("select key_name, key_json FROM grid_data where zoom_level = ? and tile_column = ? and tile_row = ?", z, x, y)
		if err != nil {
			return fmt.Errorf("cannot fetch grid data: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			err := rows.Scan(&key, &value)
			if err != nil {
				return fmt.Errorf("could not fetch grid data: %v", err)
			}
			valuejson := make(map[string]interface{})
			json.Unmarshal(value, &valuejson)
			keydata[key] = valuejson
		}

		if len(keydata) == 0 {
			return nil // there is no key data for this tile, return
		}

		var (
			zreader io.ReadCloser  // instance of zlib or gzip reader
			zwriter io.WriteCloser // instance of zlip or gzip writer
			buf     bytes.Buffer
		)
		reader := bytes.NewReader(*data)

		if tileset.utfgridCompression == ZLIB {
			zreader, err = zlib.NewReader(reader)
			if err != nil {
				return err
			}
			zwriter = zlib.NewWriter(&buf)
		} else {
			zreader, err = gzip.NewReader(reader)
			if err != nil {
				return err
			}
			zwriter = gzip.NewWriter(&buf)
		}

		var utfjson map[string]interface{}
		jsonDecoder := json.NewDecoder(zreader)
		jsonDecoder.Decode(&utfjson)
		zreader.Close()

		// splice the key data into the UTF json
		utfjson["data"] = keydata
		if err != nil {
			return err
		}

		// now re-encode to original zip encoding
		jsonEncoder := json.NewEncoder(zwriter)
		err = jsonEncoder.Encode(utfjson)
		if err != nil {
			return err
		}
		zwriter.Close()
		*data = buf.Bytes()
	}
	return nil
}

// Read the metadata table into a map, casting their values into the appropriate type
func (tileset *DB) ReadMetadata() (map[string]interface{}, error) {
	var (
		key   string
		value string
	)
	metadata := make(map[string]interface{})

	rows, err := tileset.db.Query("select * from metadata where value is not ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		rows.Scan(&key, &value)

		switch key {
		case "maxzoom", "minzoom":
			metadata[key], err = strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("cannot read metadata item %s: %v", key, err)
			}
		case "bounds", "center":
			metadata[key], err = stringToFloats(value)
			if err != nil {
				return nil, fmt.Errorf("cannot read metadata item %s: %v", key, err)
			}
		case "json":
			err = json.Unmarshal([]byte(value), &metadata)
			if err != nil {
				return nil, fmt.Errorf("unable to parse JSON metadata item: %v", err)
			}
		default:
			metadata[key] = value
		}
	}

	// Supplement missing values by inferring from available data
	_, hasMinZoom := metadata["minzoom"]
	_, hasMaxZoom := metadata["maxzoom"]
	if !(hasMinZoom && hasMaxZoom) {
		var minZoom, maxZoom int
		err := tileset.db.QueryRow("select min(zoom_level), max(zoom_level) from tiles").Scan(&minZoom, &maxZoom)
		if err != nil {
			return metadata, nil
		}
		metadata["minzoom"] = minZoom
		metadata["maxzoom"] = maxZoom
	}
	return metadata, nil
}

// TileFormatreturns the TileFormat of the DB.
func (d DB) TileFormat() TileFormat {
	return d.tileformat
}

// TileFormatString returns the string representation of the TileFormat of the DB.
func (d DB) TileFormatString() string {
	return d.tileformat.String()
}

// ContentType returns the content-type string of the TileFormat of the DB.
func (d DB) ContentType() string {
	return d.tileformat.ContentType()
}

// HasUTFGrid returns whether the DB has a UTF grid.
func (d DB) HasUTFGrid() bool {
	return d.hasUTFGrid
}

// HasUTFGridData returns whether the DB has UTF grid data.
func (d DB) HasUTFGridData() bool {
	return d.hasUTFGridData
}

// UTFGridCompression returns the compression type of the UTFGrid in the DB:
// ZLIB or GZIP
func (d DB) UTFGridCompression() TileFormat {
	return d.utfgridCompression
}

// TimeStamp returns the time stamp of the DB.
func (d DB) TimeStamp() time.Time {
	return d.timestamp
}

// Close closes the DB database connection
func (tileset *DB) Close() error {
	return tileset.db.Close()
}

// Inpsect first few bytes of byte array to determine tile format
// PBF tile format does not have a distinct signature, it will be returned
// as GZIP, and it is up to caller to determine that it is a PBF format
func detectTileFormat(data *[]byte) (TileFormat, error) {
	patterns := map[TileFormat][]byte{
		GZIP: []byte("\x1f\x8b"), // this masks PBF format too
		ZLIB: []byte("\x78\x9c"),
		PNG:  []byte("\x89\x50\x4E\x47\x0D\x0A\x1A\x0A"),
		JPG:  []byte("\xFF\xD8\xFF"),
		WEBP: []byte("\x52\x49\x46\x46\xc0\x00\x00\x00\x57\x45\x42\x50\x56\x50"),
	}

	for format, pattern := range patterns {
		if bytes.HasPrefix(*data, pattern) {
			return format, nil
		}
	}

	return UNKNOWN, errors.New("Could not detect tile format")
}
