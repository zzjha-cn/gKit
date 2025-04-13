package generic_interface

// golang不支持泛型方法，但是有的场景下，对特定方法的类型延迟绑定是必须的。
// 如在抽象一种操作数据的方法为接口，这时候肯定不知道接口调用者会传递什么数据，只有调用的时候才知道
// 为了使用方便，最好就是可以有泛型方法，调用的时候指定类型

// 在序列化的接口中，一般都是有如下三个方法：
// Version、Encode、Decode

// 首先最直观的就是这样的接口定义
type SerializeV1[T any] interface {
	Version() int
	Encode(v T) ([]byte, error)
	Decode(bys []byte) (T, error)
}

// 但是有很明显的错误，如果定义为泛型方法，那么在实现的时候就需要指定类型
// 一旦指定类型，意味着只能对一种结构序列化，而在定义序列化协议的时候，很明显是不知道具体的类型的
// 只有在协议给人使用的时候，才知道具体的方法。

// 下面这种版本才是最符合序列化协议接口的定义
type SerializeV2 interface {
	Version() int
	// 但是golang并不支持泛型方法
	// Encode[T any](v T) ([]byte, error)
	// Decode[T any](bys []byte)(T any , error)
}

// 所以一般只能写成这样
// 缺点就是在使用的时候需要自己断言具体的类型
type SerializeV3 interface {
	Version() int
	Encode(v any) ([]byte, error)
	Decode(bys []byte) (any, error)
}

// 有没有更好更方便的实现呢？
// 可以参考一下json库的反序列化方式
type SerializeV4 interface {
	Version() int
	Encode(v any) ([]byte, error)
	Decode(bys []byte, v any) error
	// Decode的时候传入一个容器，decode负责将数据反序列化后传递给这个容器
}
