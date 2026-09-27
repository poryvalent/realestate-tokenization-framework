-- ---------------------------------------------------------------------------
-- 0014_scheme_investment_manager
-- ---------------------------------------------------------------------------
--
-- Names the investor record that holds the investment manager's units.
--
-- # Why settlement needs this
--
-- The manager's 25 units are not allotted by ballot. recordImSubscription
-- credits them directly to the manager's wallet, and finaliseSettlement refuses
-- unless that wallet holds exactly im_units and is excluded from the holder
-- count. The register mirror keys every holding to an investor, so crediting the
-- manager needs to know which investor the manager is, and its active wallet is
-- the address the contract is told.
--
-- Until now nothing recorded that. A settlement endpoint would have had to take
-- the manager's wallet in the request body, which puts the one address the
-- contract excludes from the statutory count in the hands of whoever sends the
-- request.
--
-- Nullable, because a scheme exists before its manager is onboarded. Settlement
-- refuses while it is unset rather than guessing.

ALTER TABLE schemes
    ADD COLUMN im_investor_id UUID REFERENCES investors(id) ON DELETE RESTRICT;
