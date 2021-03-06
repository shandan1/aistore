# coding: utf-8

"""
    AIS

    AIStore is a scalable object-storage based caching system with Amazon and Google Cloud backends.  # noqa: E501

    OpenAPI spec version: 1.1.0
    Contact: dfc-jenkins@nvidia.com
    Generated by: https://openapi-generator.tech
"""


from __future__ import absolute_import

import unittest
from .helpers import DictParser, surpressResourceWarning

import openapi_client
from openapi_client.api.cluster_api import ClusterApi  # noqa: E501

class TestClusterApi(unittest.TestCase):
    """ClusterApi unit test stubs"""

    def setUp(self):
        surpressResourceWarning()

        configuration = openapi_client.Configuration()
        configuration.debug = False
        api_client = openapi_client.ApiClient(configuration)
        self.cluster = openapi_client.api.cluster_api.ClusterApi(api_client)
        self.daemon = openapi_client.api.daemon_api.DaemonApi(api_client)
        self.models = openapi_client.models

    def tearDown(self):
        pass

    def test_target_apis(self):
        """
        1. Get cluster map
        2. Unregister a target
        3. Verify
        4. Reregister target
        5. Verify
        """
        smap = DictParser.parse(self.daemon.get(self.models.GetWhat.SMAP))
        target =  list(smap.tmap.values())[1]
        daemon_info = self.models.Snode(target.daemon_id, public_net=target.public_net, intra_control_net=target.intra_control_net, intra_data_net=target.intra_data_net)

        self.cluster.unregister_target(daemon_info.daemon_id)
        smap = DictParser.parse(self.daemon.get(self.models.GetWhat.SMAP))
        self.assertTrue(daemon_info.daemon_id not in smap.tmap.keys(),
                        "Unregistered target still present")

        self.cluster.register_target(daemon_info)
        smap = DictParser.parse(self.daemon.get(self.models.GetWhat.SMAP))
        self.assertTrue(daemon_info.daemon_id in smap.tmap.keys(),
                        "Registered target not present")


    def test_cluster_config_api(self):
        """
        1. Set config
        2. Get config value to test that it matches the value
        :return:
        """
        input_params = self.models.InputParameters(
            self.models.Actions.SETCONFIG,
            "cksum.enable_read_range", "true")
        self.cluster.perform_operation(input_params)
        target_ids = self.daemon.get(self.models.GetWhat.SMAP)["tmap"].keys()
        target_ports = [target_id.split(":")[1] for target_id in target_ids]
        old_host = self.cluster.api_client.configuration.host
        for target_port in target_ports:
            self.daemon.api_client.configuration.host = (
                    "http://localhost:%d/v1"%int(target_port))

            config = DictParser.parse(self.daemon.get(
                self.models.GetWhat.CONFIG))
            self.assertTrue(config.cksum.enable_read_range,
                            "Set config value not getting reflected.")

        self.cluster.api_client.configuration.host = old_host
        input_params = self.models.InputParameters(
            self.models.Actions.SETCONFIG,
            "cksum.enable_read_range", "false")
        self.cluster.perform_operation(input_params)

    @unittest.skip("Running this test will cause the cluster to shutdown.")
    def test_cluster_shutdown_api(self):
        """
        Shutdown the cluster.
        :return:
        """
        input_params = self.models.InputParameters(self.models.Actions.SHUTDOWN)
        self.cluster.perform_operation(input_params)

    def test_cluster_rebalance_api(self):
        """
        Rebalance the cluster.
        :return:
        """
        input_params = self.models.InputParameters(
            self.models.Actions.REBALANCE)
        self.cluster.perform_operation(input_params)

    def test_set_primary_proxy(self):
        """
        1. Get primary proxy
        2. Make some other proxy primary
        3. Get primary proxy and verify
        4. Update primary proxy back
        :return:
        """
        smap = DictParser.parse(self.daemon.get(self.models.GetWhat.SMAP))
        primary_proxy = smap.proxy_si.daemon_id
        new_primary_proxy = None
        for proxy_id in smap.pmap.keys():
            if proxy_id != primary_proxy:
                new_primary_proxy = proxy_id
                break
        self.cluster.set_primary_proxy(new_primary_proxy)
        updated_primary_proxy = DictParser.parse(
            self.daemon.get(self.models.GetWhat.SMAP)).proxy_si.daemon_id
        self.assertEqual(updated_primary_proxy, new_primary_proxy,
                         "Primary proxy not updated")
        self.cluster.api_client.configuration.host = (
                "http://localhost:%s/v1" % new_primary_proxy[-4:])
        self.cluster.set_primary_proxy(primary_proxy)
        self.cluster.api_client.configuration.host = (
                "http://localhost:%s/v1" % primary_proxy[-4:])

    def test_cluster_stats(self):
        stats = DictParser.parse(self.cluster.get(self.models.GetWhat.STATS))
        self.assertTrue(len(stats.target.keys()) != 0,
                        "No targets retrieved while querying for stats")

    def test_cluster__xaction_stats(self):
        stats = DictParser.parse(self.cluster.get(
            what=self.models.GetWhat.XACTION,
            props=self.models.GetProps.REBALANCE))
        self.assertTrue(len(stats.target.keys()) != 0,
                        "No targets retrieved while querying for stats")

    def test_get_all_mountpaths(self):
        mountpaths = DictParser.parse(
                self.cluster.get(what=self.models.GetWhat.MOUNTPATHS))
        for target in mountpaths.targets:
            self.assertTrue(len(mountpaths.targets[target].available) > 0,
                    "Number of available mountpaths on target %s is zero."
                    % target)

if __name__ == '__main__':
    unittest.main()
