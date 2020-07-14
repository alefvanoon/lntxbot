package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/fiatjaf/go-lnurl"
	decodepay "github.com/fiatjaf/ln-decodepay"
	"github.com/fiatjaf/lntxbot/t"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/tidwall/gjson"
	"gopkg.in/jmcvetta/napping.v3"
)

type handleLNURLOpts struct {
	messageId          int
	loginSilently      bool
	payWithoutPromptIf *int64
}

func handleLNURL(u User, lnurltext string, opts handleLNURLOpts) {
	iparams, err := lnurl.HandleLNURL(lnurltext)
	if err != nil {
		if lnurlerr, ok := err.(lnurl.LNURLErrorResponse); ok {
			u.notify(t.LNURLERROR, t.T{
				"Host":   lnurlerr.URL.Host,
				"Reason": lnurlerr.Reason,
			})
		} else {
			u.notify(t.ERROR, t.T{
				"Err": fmt.Sprintf("failed to fetch lnurl params: %s", err.Error()),
			})
		}
		return
	}

	log.Debug().Interface("params", iparams).Msg("got lnurl params")

	switch params := iparams.(type) {
	case lnurl.LNURLAuthParams:
		// lnurl-auth: create a key based on the user id and sign with it
		seedhash := sha256.Sum256([]byte(fmt.Sprintf("lnurlkeyseed:%s:%d:%s", params.Host, u.Id, s.BotToken)))
		sk, pk := btcec.PrivKeyFromBytes(btcec.S256(), seedhash[:])
		k1, err := hex.DecodeString(params.K1)
		if err != nil {
			u.notify(t.ERROR, t.T{"Err": err.Error()})
			return
		}
		sig, err := sk.Sign(k1)
		if err != nil {
			u.notify(t.ERROR, t.T{"Err": err.Error()})
			return
		}

		signature := hex.EncodeToString(sig.Serialize())
		pubkey := hex.EncodeToString(pk.SerializeCompressed())

		var sentsigres lnurl.LNURLResponse
		resp, err := napping.Get(params.Callback, &url.Values{
			"sig": {signature},
			"key": {pubkey},
		}, &sentsigres, nil)
		if err != nil {
			u.notify(t.ERROR, t.T{"Err": err.Error()})
			return
		}
		if resp.Status() >= 300 {
			u.notify(t.ERROR, t.T{"Err": fmt.Sprintf(
				"Got status %d on callback %s", resp.Status(), params.Callback)})
			return
		}
		if sentsigres.Status == "ERROR" {
			u.notify(t.LNURLERROR, t.T{
				"Host":   params.Host,
				"Reason": sentsigres.Reason,
			})
			return
		}

		if !opts.loginSilently {
			u.notify(t.LNURLAUTHSUCCESS, t.T{
				"Host":      params.Host,
				"PublicKey": pubkey,
			})

			go u.track("lnurl-auth", map[string]interface{}{"domain": params.Host})
		}
	case lnurl.LNURLWithdrawResponse:
		// lnurl-withdraw: make an invoice with the highest possible value and send
		bolt11, _, _, err := u.makeInvoice(makeInvoiceArgs{
			IgnoreInvoiceSizeLimit: true,
			Msatoshi:               params.MaxWithdrawable,
			Desc:                   params.DefaultDescription,
			MessageId:              opts.messageId,
			SkipQR:                 true,
		})
		if err != nil {
			u.notify(t.ERROR, t.T{"Err": err.Error()})
			return
		}
		log.Debug().Str("bolt11", bolt11).Str("k1", params.K1).Msg("sending invoice to lnurl callback")
		var sentinvres lnurl.LNURLResponse
		resp, err := napping.Get(params.Callback, &url.Values{
			"k1": {params.K1},
			"pr": {bolt11},
		}, &sentinvres, nil)
		if err != nil {
			u.notify(t.ERROR, t.T{"Err": err.Error()})
			return
		}
		if resp.Status() >= 300 {
			u.notify(t.ERROR, t.T{"Err": fmt.Sprintf(
				"Got status %d on callback %s", resp.Status(), params.Callback)})
			return
		}
		if sentinvres.Status == "ERROR" {
			u.notify(t.LNURLERROR, t.T{
				"Host":   params.CallbackURL.Host,
				"Reason": sentinvres.Reason,
			})
			return
		}
		go u.track("lnurl-withdraw", map[string]interface{}{"sats": params.MaxWithdrawable})
	case lnurl.LNURLPayResponse1:
		// display metadata and ask for amount
		var fixedAmount int64 = 0
		if params.MaxSendable == params.MinSendable {
			fixedAmount = params.MaxSendable
		}

		go u.track("lnurl-pay", map[string]interface{}{
			"domain": params.CallbackURL.Host,
			"fixed":  float64(fixedAmount) / 1000,
			"max":    float64(params.MaxSendable) / 1000,
			"min":    float64(params.MinSendable) / 1000,
		})

		if fixedAmount > 0 &&
			opts.payWithoutPromptIf != nil &&
			fixedAmount < *opts.payWithoutPromptIf+3000 {
			lnurlpayFetchInvoiceAndPay(
				u,
				fixedAmount,
				params.Callback,
				params.EncodedMetadata,
				lnurltext,
				opts.messageId,
			)
		} else {
			tmpldata := t.T{
				"Domain":      params.CallbackURL.Host,
				"FixedAmount": float64(fixedAmount) / 1000,
				"Max":         float64(params.MaxSendable) / 1000,
				"Min":         float64(params.MinSendable) / 1000,
			}

			baseChat := tgbotapi.BaseChat{
				ChatID:           u.ChatId,
				ReplyToMessageID: opts.messageId,
			}

			if fixedAmount > 0 {
				baseChat.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData(
							translate(t.CANCEL, u.Locale),
							fmt.Sprintf("cancel=%d", u.Id)),
						tgbotapi.NewInlineKeyboardButtonData(
							translateTemplate(t.PAYAMOUNT, u.Locale,
								t.T{"Sats": float64(fixedAmount) / 1000}),
							fmt.Sprintf("lnurlpay=%d", fixedAmount)),
					),
				)
			} else {
				baseChat.ReplyMarkup = tgbotapi.ForceReply{ForceReply: true}
			}

			var chattable tgbotapi.Chattable
			tmpldata["Text"] = params.Metadata.Description()
			text := translateTemplate(t.LNURLPAYPROMPT, u.Locale, tmpldata)

			chattable = tgbotapi.MessageConfig{
				BaseChat:              baseChat,
				DisableWebPagePreview: true,
				ParseMode:             "HTML",
				Text:                  text,
			}
			if imagebytes := params.Metadata.ImageBytes(); imagebytes != nil {
				if err == nil {
					chattable = tgbotapi.PhotoConfig{
						BaseFile: tgbotapi.BaseFile{
							BaseChat: baseChat,
							File: tgbotapi.FileBytes{
								Name:  "image",
								Bytes: imagebytes,
							},
							MimeType: "image/" + params.Metadata.ImageExtension(),
						},
						ParseMode: "HTML",
						Caption:   text,
					}
				}
			}

			sent, err := tgsend(chattable)
			if err != nil {
				log.Warn().Err(err).Msg("error sending lnurl-pay message")
				return
			}

			data, _ := json.Marshal(struct {
				Type     string `json:"type"`
				Metadata string `json:"metadata"`
				URL      string `json:"url"`
				LNURL    string `json:"lnurl"`
			}{"lnurlpay", params.EncodedMetadata, params.Callback, lnurltext})
			rds.Set(fmt.Sprintf("reply:%d:%d", u.Id, sent.MessageID), data, time.Hour*1)
		}
	default:
		u.notifyAsReply(t.LNURLUNSUPPORTED, nil, opts.messageId)
	}

	return
}

func handleLNURLPayConfirmation(u User, msats int64, data gjson.Result, messageId int) {
	// get data from redis object
	callback := data.Get("url").String()
	metadata := data.Get("metadata").String()
	encodedLnurl := data.Get("lnurl").String()

	// proceed to fetch invoice and pay
	lnurlpayFetchInvoiceAndPay(u, msats, callback, metadata, encodedLnurl, messageId)
}

func lnurlpayFetchInvoiceAndPay(
	u User,
	msats int64,
	callback,
	metadata,
	encodedLnurl string,
	messageId int,
) {
	// transform lnurl into bech32ed lnurl if necessary
	encodedLnurl, _ = lnurl.LNURLEncode(encodedLnurl)

	// call callback with params and get invoice
	var res lnurl.LNURLPayResponse2
	resp, err := napping.Get(callback, &url.Values{"amount": {fmt.Sprintf("%d", msats)}}, &res, nil)
	if err != nil {
		u.notify(t.ERROR, t.T{"Err": err.Error()})
		return
	}
	if resp.Status() >= 300 {
		u.notify(t.ERROR, t.T{"Err": fmt.Sprintf(
			"Got status %d on callback %s", resp.Status(), callback)})
		return
	}
	if res.Status == "ERROR" {
		callbackURL, _ := url.Parse(callback)
		if callbackURL == nil {
			callbackURL = &url.URL{Host: "<unknown>"}
		}

		u.notify(t.LNURLERROR, t.T{
			"Host":   callbackURL.Host,
			"Reason": res.Reason,
		})
		return
	}

	log.Debug().Interface("res", res).Msg("got lnurl-pay values")

	// check invoice amount
	inv, err := decodepay.Decodepay(res.PR)
	if err != nil {
		u.notify(t.ERROR, t.T{"Err": err.Error()})
		return
	}

	if inv.DescriptionHash != calculateHash(metadata) {
		u.notify(t.ERROR, t.T{"Err": "Got invoice with wrong description_hash"})
		return
	}

	if int64(inv.MSatoshi) != msats {
		u.notify(t.ERROR, t.T{"Err": "Got invoice with wrong amount."})
		return
	}

	processingMessage := sendMessage(u.ChatId,
		res.PR+"\n\n"+translate(t.PROCESSING, u.Locale),
	)

	// pay it
	hash, err := u.payInvoice(messageId, res.PR, 0)
	if err == nil {
		deleteMessage(&processingMessage)

		// wait until lnurl-pay is paid successfully.
		go func() {
			preimage := <-waitPaymentSuccess(hash)
			bpreimage, _ := hex.DecodeString(preimage)
			callbackURL, _ := url.Parse(callback)

			// send raw metadata, for later checking with the description_hash
			file := tgbotapi.DocumentConfig{
				BaseFile: tgbotapi.BaseFile{
					BaseChat: tgbotapi.BaseChat{ChatID: u.ChatId},
					File: tgbotapi.FileBytes{
						Name:  encodedLnurl + ".json",
						Bytes: []byte(metadata),
					},
					MimeType:    "text/json",
					UseExisting: false,
				},
			}
			file.Caption = translateTemplate(t.LNURLPAYMETADATA, u.Locale, t.T{
				"Domain":         callbackURL.Host,
				"LNURL":          encodedLnurl,
				"Hash":           inv.PaymentHash,
				"HashFirstChars": inv.PaymentHash[:5],
			})
			file.ParseMode = "HTML"
			bot.Send(file)

			// notify user with success action end applicable
			if res.SuccessAction != nil {
				var text string
				var decerr error

				switch res.SuccessAction.Tag {
				case "message":
					text = res.SuccessAction.Message
				case "url":
					text = res.SuccessAction.Description
				case "aes":
					text, decerr = res.SuccessAction.Decipher(bpreimage)
				}

				// give it a time so it's the last message to be sent
				time.Sleep(2 * time.Second)

				u.notifyAsReply(t.LNURLPAYSUCCESS, t.T{
					"Domain":        callbackURL.Host,
					"Text":          text,
					"URL":           res.SuccessAction.URL,
					"DecipherError": decerr,
				}, messageId)
			}
		}()
	} else {
		u.notifyAsReply(t.ERROR, t.T{"Err": err.Error()}, processingMessage.MessageID)
	}
}
