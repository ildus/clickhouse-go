package clickhouse

import (
	"database/sql/driver"
	"fmt"
	"io"
	"reflect"

	"github.com/kshvakov/clickhouse/lib/column"
	"github.com/kshvakov/clickhouse/lib/data"
	"github.com/kshvakov/clickhouse/lib/protocol"
)

type rows struct {
	ch      *clickhouse
	columns []string

	index    int
	values   [][]driver.Value
	totals   [][]driver.Value
	extremes [][]driver.Value

	blockColumns      []column.Column
	allDataIsReceived bool
}

func (rows *rows) Columns() []string {
	if len(rows.columns) == 0 {
		rows.receiveData()
	}
	return rows.columns
}

func (rows *rows) ColumnTypeScanType(idx int) reflect.Type {
	return rows.blockColumns[idx].ScanType()
}

func (rows *rows) ColumnTypeDatabaseTypeName(idx int) string {
	return rows.blockColumns[idx].CHType()
}

func (rows *rows) Next(dest []driver.Value) error {
begin:
	if len(rows.values) <= rows.index {
		switch {
		case rows.allDataIsReceived:
			return io.EOF
		default:
			if err := rows.receiveData(); err != nil {
				return err
			}
			goto begin
		}
	}
	for i := range dest {
		dest[i] = rows.values[rows.index][i]
	}
	rows.index++
	if len(rows.values) <= rows.index {
		rows.values = nil
		for !(rows.allDataIsReceived || len(rows.values) != 0) {
			if err := rows.receiveData(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rows *rows) HasNextResultSet() bool {
	return len(rows.totals) != 0 || len(rows.extremes) != 0
}

func (rows *rows) NextResultSet() error {
	switch {
	case len(rows.totals) != 0:
		for _, value := range rows.totals {
			rows.values = append(rows.values, value)
		}
		rows.index = 0
		rows.totals = nil
	case len(rows.extremes) != 0:
		for _, value := range rows.extremes {
			rows.values = append(rows.values, value)
		}
		rows.index = 0
		rows.extremes = nil
	default:
		return io.EOF
	}
	return nil
}

func (rows *rows) receiveData() error {
	for {
		packet, err := rows.ch.decoder.Uvarint()
		if err != nil {
			return err
		}
		switch packet {
		case protocol.ServerException:
			rows.ch.logf("[receive data] <- exception")
			return rows.ch.exception(rows.ch.decoder)
		case protocol.ServerProgress:
			progress, err := rows.ch.progress(rows.ch.decoder)
			if err != nil {
				return err
			}
			rows.ch.logf("[receive data] <- progress: rows=%d, bytes=%d, total rows=%d",
				progress.rows,
				progress.bytes,
				progress.totalRows,
			)
		case protocol.ServerProfileInfo:
			profileInfo, err := rows.ch.profileInfo(rows.ch.decoder)
			if err != nil {
				return err
			}
			rows.ch.logf("[receive data] <- profiling: rows=%d, bytes=%d, blocks=%d", profileInfo.rows, profileInfo.bytes, profileInfo.blocks)
		case protocol.ServerData, protocol.ServerTotals, protocol.ServerExtremes:
			block, err := rows.ch.readBlock(rows.ch.decoder)
			if err != nil {
				return err
			}
			rows.ch.logf("[receive data] <- data: packet=%d, columns=%d, rows=%d", packet, block.NumColumns, block.NumRows)

			if len(rows.columns) == 0 && len(block.Columns) != 0 {
				rows.columns = block.ColumnNames()
				rows.blockColumns = block.Columns
				if block.NumRows == 0 {
					return nil
				}
			}
			values := convertBlockToDriverValues(block)
			switch block.Reset(); packet {
			case protocol.ServerData:
				rows.index = 0
				rows.values = values
			case protocol.ServerTotals:
				rows.totals = values
			case protocol.ServerExtremes:
				rows.extremes = values
			}
			if len(rows.values) != 0 {
				return nil
			}
		case protocol.ServerEndOfStream:
			rows.allDataIsReceived = true
			rows.ch.logf("[receive data] <- end of stream")
			return nil
		default:
			rows.ch.conn.Close()
			rows.ch.logf("[receive data] unexpected packet [%d]", packet)
			return fmt.Errorf("unexpected packet [%d] from server", packet)
		}
	}
}

func (rows *rows) Close() error {
	return nil
}

func convertBlockToDriverValues(block *data.Block) [][]driver.Value {
	values := make([][]driver.Value, 0, int(block.NumRows))
	for rowNum := 0; rowNum < int(block.NumRows); rowNum++ {
		row := make([]driver.Value, 0, block.NumColumns)
		for columnNum := 0; columnNum < int(block.NumColumns); columnNum++ {
			row = append(row, block.Values[columnNum][rowNum])
		}
		values = append(values, row)
	}
	return values
}
