package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/InjectiveLabs/injective-core/injective-chain/app"
	"github.com/cosmos/cosmos-sdk/x/auth/legacy/legacytx"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	injective_accounts_rpc "github.com/InjectiveLabs/sdk-go/exchange/accounts_rpc/pb"
	injective_meta_rpc "github.com/InjectiveLabs/sdk-go/exchange/meta_rpc/pb"
	injective_spot_rpc "github.com/InjectiveLabs/sdk-go/exchange/spot_exchange_rpc/pb"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"

	gatewaypb "github.com/qwerty-exchange/injective-ccxt/proto/gateway"
)

type server struct {
	gatewaypb.UnsafeGatewayServer
}

func NewServer() *server {
	return &server{}
}

// TxAmino implements the ServiceServer.TxEncodeAmino RPC method.
func (s *server) TxAmino(ctx context.Context, req *gatewaypb.TxAminoRequest) (*gatewaypb.TxAminoResponse, error) {
	if len(req.TxAmino) == 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid empty tx json")
	}

	encodingConfig := app.MakeEncodingConfig()
	cdc := encodingConfig.Amino

	var stdTx legacytx.StdTx
	err := cdc.UnmarshalJSON(req.TxAmino, &stdTx)
	if err != nil {
		return nil, err
	}

	encodedBytes, err := cdc.Marshal(stdTx)
	if err != nil {
		return nil, err
	}

	err = cdc.Unmarshal(encodedBytes, &stdTx)
	if err != nil {
		return nil, err
	}

	txBuilder := encodingConfig.TxConfig.NewTxBuilder()
	txBuilder.SetMsgs(stdTx.Msgs...)
	txBuilder.SetMemo(stdTx.Memo)

	sign, _ := legacytx.StdSignatureToSignatureV2(cdc, stdTx.Signatures[0])
	sign.Sequence = req.Sequence
	txBuilder.SetSignatures(sign)
	txBuilder.SetGasLimit(stdTx.Fee.Gas)
	txBuilder.SetFeeAmount(stdTx.Fee.Amount)

	r, _ := encodingConfig.TxConfig.TxEncoder()(txBuilder.GetTx())

	conn, err := grpc.Dial(
		"k8s.mainnet.chain.grpc.injective.network:443",
		grpc.WithTransportCredentials(LoadTlsCert("./cert/mainnet.crt", "tcp://k8s.mainnet.chain.grpc.injective.network:443")),
	)
	if err != nil {
		log.Fatalln("Failed to dial server:", err)
	}

	txService := txtypes.NewServiceClient(conn)
	response, err := txService.BroadcastTx(ctx, &txtypes.BroadcastTxRequest{Mode: txtypes.BroadcastMode_BROADCAST_MODE_BLOCK, TxBytes: r})
	if err != nil {
		log.Fatalln("Failed to dial server:", err)
	}
	return &gatewaypb.TxAminoResponse{
		TxResponse: response.TxResponse,
	}, err
}

func main() {

	// Create a listener on TCP port
	lis, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatalln("Failed to listen:", err)
	}

	// Create a gRPC server object
	s := grpc.NewServer()
	// Attach the Greeter service to the server
	gatewaypb.RegisterGatewayServer(s, &server{})
	// Serve gRPC server
	log.Println("Serving gRPC on 0.0.0.0:8080")
	go func() {
		log.Fatalln(s.Serve(lis))
	}()

	// Create a client connection to the gRPC server we just started
	// This is where the gRPC-Gateway proxies the requests
	conn, err := grpc.DialContext(
		context.Background(),
		"0.0.0.0:8080",
		grpc.WithBlock(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalln("Failed to dial server:", err)
	}
	gwmux := runtime.NewServeMux()

	err = gatewaypb.RegisterGatewayHandler(context.Background(), gwmux, conn)
	if err != nil {
		log.Fatalln("Failed to register gateway:", err)
	}

	conn, err = grpc.DialContext(
		context.Background(),
		"k8s.mainnet.exchange.grpc.injective.network:443",
		grpc.WithTransportCredentials(LoadTlsCert("./cert/mainnet.crt", "tcp://k8s.mainnet.exchange.grpc.injective.network:443")),
	)
	if err != nil {
		log.Fatalln("Failed to dial server:", err)
	}

	// Register Greeter
	err = injective_meta_rpc.RegisterInjectiveMetaRPCHandler(context.Background(), gwmux, conn)
	if err != nil {
		log.Fatalln("Failed to register gateway:", err)
	}

	err = injective_spot_rpc.RegisterInjectiveSpotExchangeRPCHandler(context.Background(), gwmux, conn)
	if err != nil {
		log.Fatalln("Failed to register gateway:", err)
	}

	err = injective_accounts_rpc.RegisterInjectiveAccountsRPCHandler(context.Background(), gwmux, conn)
	if err != nil {
		log.Fatalln("Failed to register gateway:", err)
	}

	gwServer := &http.Server{
		Addr:    ":8090",
		Handler: gwmux,
	}

	log.Println("Serving gRPC-Gateway on http://0.0.0.0:8090")
	log.Fatalln(gwServer.ListenAndServe())
}

func LoadTlsCert(path string, serverName string) credentials.TransportCredentials {
	if path == "" {
		return nil
	}

	// build cert obj
	rootCert, err := ioutil.ReadFile(path)
	if err != nil {
		fmt.Println(err, "cannot load tls cert from path")
		return nil
	}
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(rootCert) {
		fmt.Println(err, "failed to add server CA's certificate")
		return nil
	}
	// get domain from tcp://domain:port
	domain := strings.Split(serverName, ":")[1][2:]
	config := &tls.Config{
		RootCAs:    certPool,
		ServerName: domain,
	}
	return credentials.NewTLS(config)
}
