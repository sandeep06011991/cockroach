// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package testdata

import "errors"

type Selfish interface {
	//contract:retlint
	Self() error
}

type BadError struct{}

func (*BadError) Error() string { return "Bad" }
func (e *BadError) Self() error { return e }

type GoodPtrError struct{}

func (*GoodPtrError) Error() string { return "Good" }
func (e *GoodPtrError) Self() error { return e }

type GoodValError struct{}

func (GoodValError) Error() string { return "Good" }

type ReturnsGood interface {
	// All known implementors of this method return a good value.
	//contract:retlint
	error() error
}

type ReturnsGoodImpl struct{}

func (ReturnsGoodImpl) error() error {
	return &GoodPtrError{}
}

type ReturnsGoodImpl2 struct{}

func (ReturnsGoodImpl2) error() error { return GoodValError{} }

var (
	_ error       = &GoodPtrError{}
	_ error       = GoodValError{}
	_ Selfish     = &BadError{}
	_ Selfish     = &GoodPtrError{}
	_ ReturnsGood = ReturnsGoodImpl{}
	_ ReturnsGood = ReturnsGoodImpl2{}
)

//contract:retlint
func DirectBad() error {
	return errors.New("nope")
}

//contract:retlint
func DirectGood() error {
	switch choose() {
	case 0:
		return GoodValError{}
	case 1:
		return &GoodPtrError{}
	default:
		panic("fake code")
	}
}

//contract:retlint
func DirectTupleBad() (int, error) {
	return 0, errors.New("nope")
}

//contract:retlint
func DirectTupleBadCaller() error {
	_, err := DirectTupleBad()
	return err
}

//contract:retlint
func DirectTupleBadChain() (int, error) {
	x, err := DirectTupleBad()
	return x + 1, err
}

//contract:retlint
func EnsureGoodValWithCommaOk(err error) error {
	if tested, ok := err.(GoodValError); ok {
		return tested
	}
	return GoodValError{}
}

//contract:retlint
func ExplictReturnVarNoOp() (err error) {
	return
}

//contract:retlint
func ExplicitReturnVarBad() (err error) {
	err = DirectBad()
	return
}

//contract:retlint
func ExplicitReturnVarGood() (err error) {
	err = DirectGood()
	return
}

//contract:retlint
func ExplicitReturnVarPhiBad() (err error) {
	switch choose() {
	case 0:
		err = DirectBad()
	case 1:
		err = DirectGood()
	}
	return
}

//contract:retlint
func ExplicitReturnVarPhiGood() (err error) {
	switch choose() {
	case 0:
		err = DirectGood()
	case 1:
		err = &GoodValError{}
	case 2:
		err = &GoodPtrError{}
	}
	return
}

//contract:retlint
func EnsureGoodValWithSwitch(err error) error {
	switch t := err.(type) {
	case GoodValError:
		return t
	case *GoodPtrError:
		return t
	default:
		return GoodValError{}
	}
}

//contract:retlint
func MakesIndirectCall(fn func() error) error {
	return fn()
}

//contract:retlint
func MakesInterfaceCallBad(g Selfish) error {
	return g.Self()
}

//contract:retlint
func MakesInterfaceCallGood(g ReturnsGood) error {
	return g.error()
}

//contract:retlint
func NoopGood() {}

//contract:retlint
func NoopCallGood() {
	NoopGood()
}

//contract:retlint
func PhiBad() error {
	var ret error
	switch choose() {
	case 0:
		ret = GoodValError{}
	case 1:
		ret = &GoodPtrError{}
	case 2:
		ret = DirectGood()
	case 3:
		ret = DirectBad()
	default:
		panic("fake code")
	}
	return ret
}

//contract:retlint
func PhiGood() error {
	var ret error
	switch choose() {
	case 0:
		ret = GoodValError{}
	case 1:
		ret = &GoodPtrError{}
	case 2:
		ret = DirectGood()
	default:
		panic("fake code")
	}
	return ret
}

// ShortestWhyPath demonstrates that we choose the shortest "why" path.
//contract:retlint
func ShortestWhyPath() error {
	switch choose() {
	case 0:
		return &BadError{}
	case 1:
		return errors.New("shouldn't see this")
	default:
		return PhiBad()
	}
}

//contract:retlint
func ReturnNilGood() error {
	return nil
}

// Trying to run down this particular form, where we don't return the
// value of the TypeAssert has proven to be excessively convoluted
// to get right.
//contract:retlint
func TodoNoTypeInference(err error) error {
	if _, ok := err.(GoodValError); ok {
		return err
	}
	return GoodValError{}
}

//contract:retlint
func UsesSelfBad() error {
	return (&BadError{}).Self()
}

//contract:retlint
func UsesSelfGood() error {
	return (&GoodPtrError{}).Self()
}

func choose() int {
	return -1
}
