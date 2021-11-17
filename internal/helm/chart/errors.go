package chart

import (
	"errors"
	"fmt"
)

type BuildErrorReason string

func (e BuildErrorReason) Error() string {
	return string(e)
}

type BuildError struct {
	Reason error
	Err    error
}

func (e *BuildError) Error() string {
	if e.Reason == nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %s", e.Reason.Error(), e.Err.Error())
}

func (e *BuildError) Is(err error) bool {
	if e.Reason != nil && e.Reason == err {
		return true
	}
	return errors.Is(e.Err, err)
}

func (e *BuildError) Unwrap() error {
	return e.Err
}

var (
	ErrChartPull          = BuildErrorReason("chart pull error")
	ErrChartMetadataPatch = BuildErrorReason("chart metadata patch error")
	ErrValueFilesMerge    = BuildErrorReason("value files merge error")
	ErrDependencyBuild    = BuildErrorReason("dependency build error")
	ErrChartPackage       = BuildErrorReason("chart package error")
)
