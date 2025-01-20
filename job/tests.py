from django.test import TestCase
from django.test import Client
# Create your tests here.

class HomepageViewTest(TestCase):
    def setUp(self):
        self.client = Client()

    def test_view_renders_template(self):
        response = self.client.get('')
        self.assertTemplateUsed(response, 'job/homepage.html')
