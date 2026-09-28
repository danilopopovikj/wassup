from hatchet_sdk import Hatchet

hatchet = Hatchet()


@hatchet.workflow(name="billing", on_crons=["0 * * * *"])
class Billing:
    pass
