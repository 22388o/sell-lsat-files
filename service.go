package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/esimov/stackblur-go"
	"github.com/getAlby/gin-lsat/ginlsat"
	"github.com/gin-gonic/gin"
	"github.com/gofrs/uuid"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/mcnijman/go-emailaddress"
	"google.golang.org/grpc"
	"gorm.io/gorm"
)

const (
	MSAT_PER_SAT = 1000
)

type Config struct {
	AssetDirName string
	StaticDir    string
}
type Service struct {
	DB     *gorm.DB
	Config *Config
}

func (svc *Service) Home(c *gin.Context) {
	entries := &[]UploadedFileMetadata{}
	err := svc.DB.Find(entries, &UploadedFileMetadata{}).Error
	if err != nil {
		c.String(http.StatusInternalServerError, "Something went wrong")
		return
	}
	response := []IndexResponseEntry{}
	for _, e := range *entries {
		response = append(response, IndexResponseEntry{
			URL:       fmt.Sprintf("http://%s/assets/%s", c.Request.Host, e.Name),
			Name:      e.OriginalName,
			LNAddress: e.LNAddress,
			Price:     e.Price,
			Currency:  e.Currency,
		})
	}
	c.HTML(http.StatusOK, "index.html", gin.H{"Entries": response})
}

func (svc *Service) AssetHandler(c *gin.Context) {
	lsatInfo := c.Value("LSAT").(*ginlsat.LsatInfo)
	if lsatInfo.Type == ginlsat.LSAT_TYPE_PAID {
		c.File(fmt.Sprintf("%s/paid/%s", svc.Config.AssetDirName, c.Param("file")))
		return
	}
	c.File(fmt.Sprintf("%s/free/%s", svc.Config.AssetDirName, c.Param("file")))
}

func (svc *Service) BlurImg(filepath string) error {
	imagePath, _ := os.Open(filepath)
	defer imagePath.Close()
	srcImage, _, _ := image.Decode(imagePath)
	result, err := stackblur.Process(srcImage, 100)
	if err != nil {
		return err
	}
	newImage, _ := os.Create(strings.Replace(filepath, "/paid", "/free", 1))
	defer newImage.Close()
	return jpeg.Encode(newImage, result, &jpeg.Options{Quality: jpeg.DefaultQuality})
}

func (svc *Service) Uploadfile(c *gin.Context) {
	// single file
	file, _ := c.FormFile("file")
	lnaddress := c.Request.FormValue("ln_address")
	price, err := strconv.Atoi(c.Request.FormValue("price"))
	if err != nil {
		c.String(http.StatusBadRequest, "Price in sats needs to be specified")
		return
	}
	if lnaddress == "" || price <= 0 || file == nil {
		c.String(http.StatusBadRequest, "ln address, price and file must be set")
		return
	}
	uuid, err := uuid.NewV4()
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	if lnaddress == "" || price <= 0 || file == nil {
		c.String(http.StatusBadRequest, "ln address, price and file must be set")
		return
	}
	totalName := uuid.String() + "_" + file.Filename
	svc.DB.Create(&UploadedFileMetadata{
		LNAddress:    lnaddress,
		Name:         totalName,
		OriginalName: file.Filename,
		Price:        price,
		Currency:     "BTC",
	})
	// Upload the file to specific dst.
	err = c.SaveUploadedFile(file, fmt.Sprintf("%s/paid/%s", svc.Config.AssetDirName, totalName))
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	//blur the file and save the blurred file as well
	err = svc.BlurImg(fmt.Sprintf("%s/paid/%s", svc.Config.AssetDirName, totalName))
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]string{
		"msg": "File succesfully uploaded. You can close this page.",
		"url": fmt.Sprintf("https://%s/assets/%s", c.Request.Host, totalName),
	})
}
func (svc *Service) AddInvoice(ctx context.Context, lnReq *lnrpc.Invoice, httpReq *http.Request, options ...grpc.CallOption) (*lnrpc.AddInvoiceResponse, error) {
	fetched := &UploadedFileMetadata{}
	name := strings.TrimPrefix(httpReq.URL.Path, "/assets/")
	if name == "" {
		return nil, fmt.Errorf("no filename specified")
	}
	err := svc.DB.First(fetched, &UploadedFileMetadata{
		Name: name,
	}).Error
	if err != nil {
		return nil, err
	}
	resp, err := FindLNAddress(fetched.LNAddress)
	if err != nil {
		return nil, err
	}
	inv, err := FetchInvoice(resp.Callback, fmt.Sprintf("LSAT invoice for file %s", fetched.Name), int(fetched.Price))
	if err != nil {
		return nil, err
	}
	decoded, err := zpay32.Decode(inv, &chaincfg.MainNetParams)
	if err != nil {
		return nil, err
	}
	return &lnrpc.AddInvoiceResponse{
		RHash:          decoded.PaymentHash[:],
		PaymentRequest: inv,
	}, nil
}

func FindLNAddress(input string) (response *LNURLPayResponse, err error) {
	emails := emailaddress.Find([]byte(input), false)
	for _, e := range emails {
		url := constructLNURL(e.LocalPart, e.Domain)
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		payload := &LNURLPayResponse{}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		if err != nil {
			return nil, err
		}
		if payload.Callback != "" {
			return payload, nil
		}
	}

	return nil, fmt.Errorf("nothing found %s", input)
}

func constructLNURL(user, host string) (result string) {
	return fmt.Sprintf("https://%s/.well-known/lnurlp/%s", host, user)
}

func FetchInvoice(callback, comment string, satAmt int) (invoice string, err error) {
	if err != nil {
		return "", err
	}
	resp, err := http.Get(fmt.Sprintf("%s?amount=%d&comment=%s", callback, satAmt*MSAT_PER_SAT, url.QueryEscape(comment)))
	if err != nil {
		return "", err
	}
	payload := &SecondaryLNURLPayResponse{}
	err = json.NewDecoder(resp.Body).Decode(payload)
	if err != nil {
		return "", err
	}

	return payload.Invoice, nil
}

type LNURLPayResponse struct {
	Callback    string `json:"callback"`
	MaxSendable int    `json:"maxSendable"`
	Metadata    string `json:"metadata"`
	MinSendable int    `json:"minSendable"`
	Tag         string `json:"tag"`
}

type SecondaryLNURLPayResponse struct {
	Invoice string `json:"pr"`
}
