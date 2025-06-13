package main

import (
        "bytes"
        "encoding/json"
        "io"
        "log"
        "net/http"
        "os"
)

var prebidURL = "http://localhost:8000/openrtb2/auction"
var jioURL = "https://mercury-dsp.jio.com/jiodsp/?spid=51"
var publisherID = ""

var adSlotMappings = map[string]map[string]string{
        "com.truecaller": {
                "banner": "6931445",
                "video":  "6931468",
                "native": "6931469",
        },
        "com.snapchat.android": {
                "banner": "snap_banner_slot_01",
                "video":  "snap_video_slot_02",
                "native": "snap_native_slot_03",
        },
}

var fallbackAdSlots = map[string]string{
        "banner": "default_banner_slot",
        "video":  "default_video_slot",
        "native": "default_native_slot",
}

func main() {
        // Setup file logging
        logFile, err := os.OpenFile("proxy.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
        if err != nil {
                log.Fatalf("❌ Failed to open log file: %v", err)
        }
        log.SetOutput(logFile)
        log.Println("🚀 Proxy server started. Logging to proxy.log")

        http.HandleFunc("/openrtb2/auction", proxyHandler)
        log.Fatal(http.ListenAndServe(":8080", nil))
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                http.Error(w, "Only POST requests are allowed", http.StatusMethodNotAllowed)
                return
        }

        body, err := io.ReadAll(r.Body)
        if err != nil {
                http.Error(w, "Failed to read request", http.StatusInternalServerError)
                return
        }
        defer r.Body.Close()

        var req map[string]interface{}
        if err := json.Unmarshal(body, &req); err != nil {
                http.Error(w, "Invalid JSON in request", http.StatusBadRequest)
                log.Printf("❌ Invalid JSON: %v", err)
                return
        }

        // Log app bundle
        bundle := "UNKNOWN_BUNDLE"
        if app, ok := req["app"].(map[string]interface{}); ok {
                if b, ok := app["bundle"].(string); ok {
                        bundle = b
                }
        }
        log.Printf("📥 Incoming request from bundle: %s", bundle)

        // Log detected ad types
        logAdTypes(req)

        // Inject PubMatic
        injectPubmaticConfig(req, bundle)

        modifiedBody, err := json.Marshal(req)
        if err != nil {
                http.Error(w, "Failed to marshal modified request", http.StatusInternalServerError)
                log.Printf("❌ Marshal error: %v", err)
                return
        }

        // Send requests in parallel
        pbsRespCh := make(chan []byte)
        jioRespCh := make(chan []byte)

        go func() {
                resp, err := http.Post(prebidURL, "application/json", bytes.NewBuffer(modifiedBody))
                if err != nil {
                        log.Printf("❌ Error forwarding to PBS: %v", err)
                        pbsRespCh <- nil
                        return
                }
                defer resp.Body.Close()
                respBody, _ := io.ReadAll(resp.Body)
                log.Printf("✅ Response received from PubMatic: %s", string(respBody))
                pbsRespCh <- respBody
        }()

        go func() {
                respBody, err := sendToJio(body)
                if err != nil {
                        log.Printf("❌ Error sending to Jio DSP: %v", err)
                        jioRespCh <- nil
                        return
                }
                log.Printf("✅ Response received from Jio: %s", string(respBody))
                jioRespCh <- respBody
        }()

        pbsResp := <-pbsRespCh
        jioResp := <-jioRespCh

        finalResp := extractHighestBid(pbsResp, jioResp)
        if finalResp == nil {
                log.Printf("⚠️ No valid bids received from either DSP")
                http.Error(w, "No valid bids received", http.StatusNoContent)
                return
        }

        w.Header().Set("Content-Type", "application/json")
        w.Write(finalResp)
}

func injectPubmaticConfig(req map[string]interface{}, bundle string) {
        impList, ok := req["imp"].([]interface{})
        if !ok {
                log.Println("⚠️ No 'imp' list found in request")
                return
        }

        for i, item := range impList {
                imp, ok := item.(map[string]interface{})
                if !ok {
                        continue
                }

                adType := getAdType(imp)
                slot := getAdSlot(bundle, adType)

                ext := getOrCreateMap(imp, "ext")
                prebid := getOrCreateMap(ext, "prebid")
                bidder := getOrCreateMap(prebid, "bidder")

                bidder["pubmatic"] = map[string]interface{}{
                        "publisherId": publisherID,
                        "adSlot":      slot,
                }

                log.Printf("🧩 Injected PubMatic for adType=%s with adSlot=%s", adType, slot)

                prebid["bidder"] = bidder
                ext["prebid"] = prebid
                imp["ext"] = ext
                impList[i] = imp
        }

        req["imp"] = impList
}

func sendToJio(originalBody []byte) ([]byte, error) {
        jioReq := make(map[string]interface{})
        if err := json.Unmarshal(originalBody, &jioReq); err != nil {
                return nil, err
        }

        ext := getOrCreateMap(jioReq, "ext")
        ext["ssp"] = "abc"
        ext["spid"] = "51"
        jioReq["ext"] = ext

        log.Printf("📦 Injected Jio ext: %+v", ext)

        jioBody, err := json.Marshal(jioReq)
        if err != nil {
                return nil, err
        }

        resp, err := http.Post(jioURL, "application/json", bytes.NewBuffer(jioBody))
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()
        return io.ReadAll(resp.Body)
}

func extractHighestBid(pbsResp, jioResp []byte) []byte {
        var pbs map[string]interface{}
        var jio map[string]interface{}

        var pbsBid float64 = 0
        var jioBid float64 = 0
        var winnerResp []byte

        if json.Unmarshal(pbsResp, &pbs) == nil {
                if bid := extractFirstBid(pbs); bid != nil {
                        pbsBid = bid["price"].(float64)
                        winnerResp = pbsResp
                }
        }

        if json.Unmarshal(jioResp, &jio) == nil {
                if bid := extractFirstBid(jio); bid != nil {
                        jioBid = bid["price"].(float64)
                        if jioBid > pbsBid {
                                winnerResp = jioResp
                        }
                }
        }

        log.Printf("🏁 Final Bids -> PubMatic: %.2f, Jio: %.2f | Winner: %s", pbsBid, jioBid,
                func() string {
                        if jioBid > pbsBid {
                                return "Jio"
                        } else if pbsBid > 0 {
                                return "PubMatic"
                        }
                        return "None"
                }(),
        )
        return winnerResp
}

func extractFirstBid(resp map[string]interface{}) map[string]interface{} {
        if seatBid, ok := resp["seatbid"].([]interface{}); ok && len(seatBid) > 0 {
                if bidArr, ok := seatBid[0].(map[string]interface{})["bid"].([]interface{}); ok && len(bidArr) > 0 {
                        if bid, ok := bidArr[0].(map[string]interface{}); ok {
                                return bid
                        }
                }
        }
        return nil
}

func getAdSlot(bundle, adType string) string {
        if bundleMap, ok := adSlotMappings[bundle]; ok {
                if slot, ok := bundleMap[adType]; ok {
                        return slot
                }
        }
        return fallbackAdSlots[adType]
}

func getAdType(imp map[string]interface{}) string {
        if _, ok := imp["video"]; ok {
                return "video"
        } else if _, ok := imp["banner"]; ok {
                return "banner"
        } else if _, ok := imp["native"]; ok {
                return "native"
        }
        return "unknown"
}

func logAdTypes(req map[string]interface{}) {
        if imps, ok := req["imp"].([]interface{}); ok {
                for _, i := range imps {
                        if imp, ok := i.(map[string]interface{}); ok {
                                log.Printf("📊 Impression type detected: %s", getAdType(imp))
                        }
                }
        }
}

func getOrCreateMap(parent map[string]interface{}, key string) map[string]interface{} {
        if val, ok := parent[key]; ok {
                if m, ok := val.(map[string]interface{}); ok {
                        return m
                }
        }
        newMap := make(map[string]interface{})
        parent[key] = newMap
        return newMap
}
