package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"

	"github.com/exchangedataset/streamcommons/jsonstructs"
)

var bitflyerChannelPrefixes = []string{
	"lightning_executions_",
	"lightning_board_snapshot_",
	"lightning_board_",
	"lightning_ticker_",
}

func bitflyerFetchMarkets() (productCodes []string, err error) {
	// Fetch what markets they have
	res, err := http.Get("https://api.bitflyer.com/v1/markets")
	defer func() {
		serr := res.Body.Close()
		if serr != nil {
			if err != nil {
				err = fmt.Errorf("%v, original error was: %v", serr, err)
			} else {
				err = serr
			}
		}
	}()
	if err != nil {
		return
	}
	var resBytes []byte
	resBytes, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return
	}
	markets := make([]struct {
		ProductCode string `json:"product_code"`
	}, 0, 100)
	err = json.Unmarshal(resBytes, &markets)
	if err != nil {
		return
	}
	// Response have the format of [{'product_code':'BTC_JPY'},{...}...]
	// Produce an array of product_code
	productCodes = make([]string, len(markets))
	for i, market := range markets {
		productCodes[i] = market.ProductCode
	}
	return
}

type bitflyerDump struct {
	productCodes []string
}

func (d *bitflyerDump) Subscribe() ([][]byte, error) {
	subs := make([][]byte, 0, 100)
	substruct := new(jsonstructs.BitflyerSubscribe)
	substruct.Initialize()
	i := 0
	for _, productCode := range d.productCodes {
		for _, prefix := range bitflyerChannelPrefixes {
			substruct.ID = i
			channel := prefix + productCode
			substruct.Params.Channel = channel
			subMarshaled, serr := json.Marshal(substruct)
			if serr != nil {
				return nil, serr
			}
			subs = append(subs, subMarshaled)
			i++
		}
	}
	return subs, nil
}
func (d *bitflyerDump) BeforeConnect() error {
	var serr error
	d.productCodes, serr = bitflyerFetchMarkets()
	if serr != nil {
		return fmt.Errorf("fetch markets: %v", serr)
	}
	return nil
}

func dumpBitflyer(ctx context.Context, directory string, alwaysDisk bool, logger *log.Logger) error {
	return dumpNormal(ctx, "bitflyer", "wss://ws.lightstream.bitflyer.com/json-rpc", directory, alwaysDisk, logger, &bitflyerDump{})
}
