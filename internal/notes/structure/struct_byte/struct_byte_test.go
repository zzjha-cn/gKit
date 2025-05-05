package structbyte_test

/* struct与byte的相互转化 */

type DemoModel struct{
	F1 uint64 // 8字节
	F2 uint32 // 4字节
	F3 byte
}
