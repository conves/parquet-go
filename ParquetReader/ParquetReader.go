package ParquetReader

import (
	"encoding/binary"
	"git.apache.org/thrift.git/lib/go/thrift"
	"github.com/xitongsys/parquet-go/Common"
	"github.com/xitongsys/parquet-go/Marshal"
	"github.com/xitongsys/parquet-go/ParquetFile"
	"github.com/xitongsys/parquet-go/SchemaHandler"
	"github.com/xitongsys/parquet-go/parquet"
	"reflect"
	"sync"
)

type ParquetReader struct {
	SchemaHandler *SchemaHandler.SchemaHandler
	NP            int64 //parallel number
	Footer        *parquet.FileMetaData
	PFile         ParquetFile.ParquetFile

	ColumnBuffers map[string]*ColumnBufferType
}

//Create a parquet reader
func NewParquetReader(pFile ParquetFile.ParquetFile, np int64) (*ParquetReader, error) {
	var err error
	res := new(ParquetReader)
	res.NP = np
	res.PFile = pFile
	res.ReadFooter()
	res.ColumnBuffers = make(map[string]*ColumnBufferType)
	res.SchemaHandler = SchemaHandler.NewSchemaHandlerFromSchemaList(res.Footer.GetSchema())
	for i := 0; i < len(res.SchemaHandler.SchemaElements); i++ {
		schema := res.SchemaHandler.SchemaElements[i]
		pathStr := res.SchemaHandler.IndexMap[int32(i)]
		numChildren := schema.GetNumChildren()
		if numChildren == 0 {
			res.ColumnBuffers[pathStr], err = NewColumnBuffer(pFile, res.Footer, res.SchemaHandler, pathStr)
			if err != nil {
				return res, err
			}
		}
	}
	return res, err
}

func (self *ParquetReader) GetNumRows() int64 {
	return self.Footer.GetNumRows()
}

//Get the footer size
func (self *ParquetReader) GetFooterSize() uint32 {
	buf := make([]byte, 4)
	self.PFile.Seek(-8, 2)
	self.PFile.Read(buf)
	size := binary.LittleEndian.Uint32(buf)
	return size
}

//Read footer from parquet file
func (self *ParquetReader) ReadFooter() {
	size := self.GetFooterSize()
	self.PFile.Seek(int(-(int64)(8+size)), 2)
	self.Footer = parquet.NewFileMetaData()
	pf := thrift.NewTCompactProtocolFactory()
	protocol := pf.GetProtocol(thrift.NewStreamTransportR(self.PFile))
	self.Footer.Read(protocol)
}

func (self *ParquetReader) Read(dstInterface interface{}) {
	tmap := make(map[string]*Common.Table)
	locker := new(sync.Mutex)
	ot := reflect.TypeOf(dstInterface).Elem().Elem()
	num := reflect.ValueOf(dstInterface).Elem().Len()

	doneChan := make(chan int, self.NP)
	taskChan := make(chan string, len(self.ColumnBuffers))

	stopFlag := false
	for i := int64(0); i < self.NP; i++ {
		go func() {
			for !stopFlag {
				pathStr := <-taskChan
				cb := self.ColumnBuffers[pathStr]
				table, _ := cb.ReadRows(int64(num))
				locker.Lock()
				if _, ok := tmap[pathStr]; ok {
					tmap[pathStr].Merge(table)
				} else {
					tmap[pathStr] = table
				}
				locker.Unlock()
				doneChan <- 0
			}
		}()
	}
	for key, _ := range self.ColumnBuffers {
		taskChan <- key
	}
	for i := 0; i < len(self.ColumnBuffers); i++ {
		<-doneChan
	}
	stopFlag = true

	dstList := make([]interface{}, self.NP)
	delta := (int64(num) + self.NP - 1) / self.NP

	doneChan = make(chan int)
	for c := int64(0); c < self.NP; c++ {
		bgn := c * delta
		end := bgn + delta
		if end > int64(num) {
			end = int64(num)
		}
		if bgn >= int64(num) {
			bgn, end = int64(num), int64(num)
		}
		go func(b, e, index int) {
			dstList[index] = reflect.New(reflect.SliceOf(ot)).Interface()
			Marshal.Unmarshal(&tmap, b, e, dstList[index], self.SchemaHandler)
			doneChan <- 0
		}(int(bgn), int(end), int(c))
	}
	for c := int64(0); c < self.NP; c++ {
		<-doneChan
	}

	resTmp := reflect.MakeSlice(reflect.SliceOf(ot), 0, num)
	for _, dst := range dstList {
		resTmp = reflect.AppendSlice(resTmp, reflect.ValueOf(dst).Elem())
	}

	reflect.ValueOf(dstInterface).Elem().Set(resTmp)

}
