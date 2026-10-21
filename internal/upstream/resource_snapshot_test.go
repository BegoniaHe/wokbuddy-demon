package upstream

import (
	"net/http"
	"testing"
	"time"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestUserResourceSnapshotBucketsByDay 余额快照返回逐日到期批次：同日多包合并、
// 按日期升序、earliest 与批次一致（与面板「检查」同口径，issue #200）。
func TestUserResourceSnapshotBucketsByDay(t *testing.T) {
	t.Parallel()

	day1 := time.Now().Add(24 * time.Hour).Truncate(time.Second).In(softRateResetLoc)
	day2 := day1.Add(48 * time.Hour)

	client := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/v2/billing/meter/get-user-resource-summary":
			return jsonResp(200, `{"code":0,"data":{"packages":[{"remain":900,"total":1000}]}}`), nil
		case r.URL.Path == "/v2/billing/meter/get-user-resource-paid-packages":
			return jsonResp(200, `{"code":0,"data":{"Accounts":[
				{"CapacitySize":300,"CapacityRemain":300,"DeductionEndTime":`+itoa(day1.UnixMilli())+`},
				{"CapacitySize":200,"CapacityRemain":200,"DeductionEndTime":`+itoa(day1.UnixMilli())+`},
				{"CapacitySize":400,"CapacityRemain":400,"DeductionEndTime":`+itoa(day2.UnixMilli())+`}
			],"TotalCount":3}}`), nil
		default:
			return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
		}
	})

	snap, err := client.UserResourceSnapshot(&auth.Auth{AccessToken: "at", UID: "u1"}, 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if len(snap.Buckets) != 2 {
		t.Fatalf("buckets=%v want 2（同日合并）", snap.Buckets)
	}

	if snap.Buckets[0].Date != expiryBucketDate(day1) || snap.Buckets[0].Remain != 500 {
		t.Errorf("first bucket=%+v want %s/500", snap.Buckets[0], expiryBucketDate(day1))
	}

	if snap.Buckets[1].Date != expiryBucketDate(day2) || snap.Buckets[1].Remain != 400 {
		t.Errorf("second bucket=%+v want %s/400", snap.Buckets[1], expiryBucketDate(day2))
	}
}

// TestUserResourceDetailedMatchesSnapshot 老接口（薄包装）的最早批次与桶口径一致。
func TestUserResourceDetailedMatchesSnapshot(t *testing.T) {
	t.Parallel()

	day1 := time.Now().Add(24 * time.Hour).Truncate(time.Second).In(softRateResetLoc)

	client := testClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v2/billing/meter/get-user-resource-summary" {
			return jsonResp(200, `{"code":0,"data":{"packages":[{"remain":300,"total":300}]}}`), nil
		}

		if r.URL.Path == "/v2/billing/meter/get-user-resource-paid-packages" {
			return jsonResp(200, `{"code":0,"data":{"Accounts":[
				{"CapacitySize":300,"CapacityRemain":300,"DeductionEndTime":`+itoa(day1.UnixMilli())+`}
			],"TotalCount":1}}`), nil
		}

		return jsonResp(200, `{"code":0,"data":{"Accounts":[],"TotalCount":0}}`), nil
	})

	account := &auth.Auth{AccessToken: "at", UID: "u1"}

	snap, err := client.UserResourceSnapshot(account, 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	_, _, _, earliestAt, earliestRemaining, err := client.UserResourceDetailedWithExpiry(account, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}

	if len(snap.Buckets) != 1 || !earliestAt.Equal(snap.Buckets[0].ExpireAt) || earliestRemaining != snap.Buckets[0].Remain {
		t.Fatalf("earliest=%v/%d buckets=%v 不一致", earliestAt, earliestRemaining, snap.Buckets)
	}
}

// itoa 小工具（避免为一个数字引 strconv 到 . 之前）。
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}

// TestResourceAccountExpiryPackageEndTime 回退路径（get-user-resource）与「检查」
// 同优先级：仅带 PackageEndTime 的包必须按该字段取到期，不能回落到周期重置日。
func TestResourceAccountExpiryPackageEndTime(t *testing.T) {
	t.Parallel()

	end := time.Now().Add(48 * time.Hour).Truncate(time.Second).In(softRateResetLoc)
	acct := &resourceAccount{
		CapacityType:   1,
		PackageEndTime: end.Format(packageEndLayout),
		CycleEndTime:   end.AddDate(0, 1, 0).Format(packageEndLayout),
	}

	got, ok := resourceAccountExpiry(acct)
	if !ok || !got.Equal(end) {
		t.Fatalf("expiry=%v ok=%v want %v（PackageEndTime 优先于 CycleEndTime）", got, ok, end)
	}
}
