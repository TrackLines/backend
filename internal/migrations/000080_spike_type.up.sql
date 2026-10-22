-- Spikes (TL-502): a timeboxed investigation ticket. Unlike epics, a spike is normal workable work —
-- it lives on a board and in a sprint and takes an estimate; it just also carries a timebox (000081).
ALTER TYPE ticket_type ADD VALUE IF NOT EXISTS 'spike';
