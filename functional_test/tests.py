from selenium import webdriver
from django.test import LiveServerTestCase
from selenium.webdriver.common.by import By 
from time import sleep


class PrintJobs(LiveServerTestCase):
    
    def setUp(self):
        self.browser = webdriver.Firefox()
    
    def tearDown(self):
        self.browser.quit()

    def test_can_send_print_jobs(self):

        # User goes to the website
        self.browser.get(self.live_server_url)

        self.assertEqual("NetPr",self.browser.title)

        # User types a title
        title_box = self.browser.find_element(By.ID, "inp-title")
        title_box.send_keys("Test Print")

        # User types a password "P@ssw0rd"
        password_box = self.browser.find_element(By.ID, "inp-passwd")
        password_box.send_keys("P@ssw0rd")

        # User selects a file

        # User clicks print
        print_button = self.browser.find_element(By.ID,"inp-submit")
        print_button.click()

        # User sees that it was approved with a "Print was Successfull" page 
        ret_msg = self.browser.find_element(By.ID,"response-text").text
        self.assertEqual(ret_msg,"Print Successful")
        # User is redirected to the homepage