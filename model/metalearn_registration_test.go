package model

import (
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
)

func TestMetaLearnRegistrationOnceFrozenAndNotPayment(t *testing.T) {
	setupMetaLearnTest(t)
	require.NoError(t, DB.AutoMigrate(&MetaLearnRegistrationReward{}))
	_, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := MetaLearnRegistrationRequest{BusinessUserID: metalearnTestUUID, RewardKey: "registration-v1", QuotaAmount: 500000}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := GrantMetaLearnRegistrationReward(r); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	view, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.EqualValues(t, 500000, view.QuotaBalance)
	r.QuotaAmount = 1000000
	_, err = GrantMetaLearnRegistrationReward(r)
	require.ErrorIs(t, err, ErrMetaLearnIdentity)
	r.QuotaAmount = 500000
	r.RewardKey = "registration-v2"
	_, err = GrantMetaLearnRegistrationReward(r)
	require.ErrorIs(t, err, ErrMetaLearnInvalid)
	var count int64
	require.NoError(t, DB.Model(&TopUp{}).Count(&count).Error)
	require.Zero(t, count)
}
func TestMetaLearnRegistrationDisabledStaysDisabled(t *testing.T) {
	setupMetaLearnTest(t)
	require.NoError(t, DB.AutoMigrate(&MetaLearnRegistrationReward{}))
	_, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := MetaLearnRegistrationRequest{BusinessUserID: metalearnTestUUID, RewardKey: "registration-v1"}
	_, err = GrantMetaLearnRegistrationReward(r)
	require.NoError(t, err)
	r.QuotaAmount = 500000
	_, err = GrantMetaLearnRegistrationReward(r)
	require.ErrorIs(t, err, ErrMetaLearnIdentity)
	view, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Zero(t, view.QuotaBalance)
}
