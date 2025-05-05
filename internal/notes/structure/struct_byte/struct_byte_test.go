package structbyte_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

/* struct与[]byte的相互转化 */
// 要么强制将strcut转化为底层的[]uint
// 要么自定义序列化

type DemoModel struct {
	F1 uint64 // 8byte
	F2 uint32 // 4byte
	F3 byte   // 1 byte
}

func TestJsonMarshal(t *testing.T) {
	d := DemoModel{}

	bys, err := json.Marshal(d)
	assert.Nil(t, err)

	fmt.Println("size ", unsafe.Sizeof(d))
	fmt.Println("json size ", len(bys))
	//	size  16
	//  json size  22
	// 可以看到，json序列化后，占用了很多字节
}

// 强制转化 struct - byte
func TestForce2byte(t *testing.T) {
	d := DemoModel{
		F1: 123,
		F2: 43,
		F3: 12,
	}

	size := unsafe.Sizeof(d)
	bys := (*[4096]byte)(unsafe.Pointer(&d))[:size]

	fmt.Println("size bys", size, len(bys))

	// 写入到IO.比如文件中
	var ioTo = bytes.Buffer{}
	_, err := ioTo.Write(bys)
	assert.Nil(t, err)

	x, err := io.ReadAll(&ioTo)
	assert.Nil(t, err)
	eq := reflect.DeepEqual(x, bys)
	assert.Equal(t, true, eq)
}

func (m *DemoModel) Marshal() ([]byte, error) {
	// 手动将结构体序列化
	// 使用大端序,将结构体各个属性加入到字节数组中
	buf := make([]byte, 0, 10)
	buf = binary.BigEndian.AppendUint64(buf, m.F1)
	// binary.BigEndian.PutUint16(buf, uint16(m.F2))
	buf = binary.BigEndian.AppendUint32(buf, m.F2)
	buf = append(buf, m.F3)

	return buf, nil
}

func (m *DemoModel) Unmarshal(bys []byte) error {
	m.F1 = binary.BigEndian.Uint64(bys[:8])
	m.F2 = binary.BigEndian.Uint32(bys[8:12])
	m.F3 = bys[12]
	return nil
}

func TestMarshaler(t *testing.T) {
	d := DemoModel{
		F1: 33,
		F2: 44,
		F3: 12,
	}

	bys, err := d.Marshal()
	assert.Nil(t, err)

	fmt.Println(len(bys))

	d1 := DemoModel{}
	d1.Unmarshal(bys)

	eq := reflect.DeepEqual(d1, d)
	assert.True(t, eq)

}
