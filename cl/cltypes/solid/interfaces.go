package solid

type Uint64Slice interface {
	Clear()
	CopyTo(Uint64Slice)
	Range(fn func(index int, value uint64, length int) bool)
	Pop() uint64
	Append(v uint64)
	Get(index int) uint64
	Set(index int, v uint64)
	Length() int
	Cap() int
	HashSSZTo(xs []byte) error
}
