package base

import "google.golang.org/grpc"

type Client struct {
	*grpc.ClientConn
}

func NewRpcClient(taget string) (*Client, error) {
	res := &Client{}
	conn, err := grpc.NewClient(taget, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	res.ClientConn = conn
	return res, nil
}
