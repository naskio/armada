// Code generated by go-swagger; DO NOT EDIT.

package models

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"context"

	"github.com/go-openapi/errors"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/go-openapi/validate"
)

// Group group
//
// swagger:model group
type Group struct {

	// aggregates
	// Required: true
	Aggregates map[string]string `json:"aggregates"`

	// count
	// Required: true
	Count int64 `json:"count"`

	// name
	// Required: true
	// Min Length: 1
	Name string `json:"name"`
}

// Validate validates this group
func (m *Group) Validate(formats strfmt.Registry) error {
	var res []error

	if err := m.validateAggregates(formats); err != nil {
		res = append(res, err)
	}

	if err := m.validateCount(formats); err != nil {
		res = append(res, err)
	}

	if err := m.validateName(formats); err != nil {
		res = append(res, err)
	}

	if len(res) > 0 {
		return errors.CompositeValidationError(res...)
	}
	return nil
}

func (m *Group) validateAggregates(formats strfmt.Registry) error {

	if err := validate.Required("aggregates", "body", m.Aggregates); err != nil {
		return err
	}

	return nil
}

func (m *Group) validateCount(formats strfmt.Registry) error {

	if err := validate.Required("count", "body", int64(m.Count)); err != nil {
		return err
	}

	return nil
}

func (m *Group) validateName(formats strfmt.Registry) error {

	if err := validate.RequiredString("name", "body", m.Name); err != nil {
		return err
	}

	if err := validate.MinLength("name", "body", m.Name, 1); err != nil {
		return err
	}

	return nil
}

// ContextValidate validates this group based on context it is used
func (m *Group) ContextValidate(ctx context.Context, formats strfmt.Registry) error {
	return nil
}

// MarshalBinary interface implementation
func (m *Group) MarshalBinary() ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return swag.WriteJSON(m)
}

// UnmarshalBinary interface implementation
func (m *Group) UnmarshalBinary(b []byte) error {
	var res Group
	if err := swag.ReadJSON(b, &res); err != nil {
		return err
	}
	*m = res
	return nil
}